package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"unicode/utf8"
)

const (
	maxSettlementOperations   = 500
	maxSettlementBytes        = 256 * 1024
	maxSettlementParticipants = 64
)

type silverChange struct {
	UserID uint64 `json:"user_id"`
	Debit  int64  `json:"debit"`
	Credit int64  `json:"credit"`
}

type silverOperation struct {
	ID        string          `json:"operation_id"`
	Business  string          `json:"business"`
	Reference string          `json:"business_ref"`
	Changes   []silverChange  `json:"changes"`
	Detail    json.RawMessage `json:"detail"`
}

func (operation silverOperation) digest() [32]byte {
	encoded, _ := json.Marshal(operation)
	return sha256.Sum256(encoded)
}

type settlementBatch struct {
	ID         string            `json:"batch_id"`
	Kind       string            `json:"kind"`
	Operations []silverOperation `json:"operations"`
}

func (batch settlementBatch) digest() [32]byte {
	encoded, _ := json.Marshal(batch)
	return sha256.Sum256(encoded)
}

type walletBalance struct {
	UserID  uint64 `json:"user_id"`
	Amount  int64  `json:"amount"`
	Version uint64 `json:"version"`
}

func (balance walletBalance) wire() map[string]any {
	return map[string]any{"user_id": balance.UserID, "amount": balance.Amount, "version": balance.Version}
}

type settlementResult struct {
	OperationID string `json:"operation_id"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
	OriginBatch string `json:"origin_batch"`
}

func (result settlementResult) wire() map[string]any {
	return map[string]any{"operation_id": result.OperationID, "status": result.Status, "reason": result.Reason, "origin_batch": result.OriginBatch}
}

type settlementReceipt struct {
	BatchID  string             `json:"batch_id"`
	Results  []settlementResult `json:"results"`
	Balances []walletBalance    `json:"balances"`
}

func (receipt settlementReceipt) wire() map[string]any {
	results, balances := make([]any, len(receipt.Results)), make([]any, len(receipt.Balances))
	for i, result := range receipt.Results {
		results[i] = result.wire()
	}
	for i, balance := range receipt.Balances {
		balances[i] = balance.wire()
	}
	return map[string]any{"batch_id": receipt.BatchID, "results": results, "balances": balances}
}

func validSettlementID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for i := range value {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func exactFields(fields map[string]any, names ...string) bool {
	if len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

func validVersion(fields map[string]any) bool {
	return fields["version"] == uint64(1)
}

func decodeSettlementBatch(fields map[string]any) (settlementBatch, error) {
	id, _ := fields["batch_id"].(string)
	values, ok := fields["operations"].([]any)
	batch := settlementBatch{ID: id, Kind: "batch"}
	if !exactFields(fields, "version", "batch_id", "operations") || !validVersion(fields) || !validSettlementID(id) || !ok || len(values) == 0 || len(values) > maxSettlementOperations {
		return batch, errInvalidSettlement
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		entry, ok := value.(map[string]any)
		if !ok {
			return batch, errInvalidSettlement
		}
		operation, err := decodeSilverOperation(entry, false, false)
		if err != nil || seen[operation.ID] {
			return batch, errInvalidSettlement
		}
		seen[operation.ID] = true
		batch.Operations = append(batch.Operations, operation)
	}
	return boundedBatch(batch, fields)
}

func decodeSettlementSingle(fields map[string]any) (settlementBatch, error) {
	operation, err := decodeSilverOperation(fields, false, true)
	if err != nil {
		return settlementBatch{}, err
	}
	return boundedBatch(settlementBatch{ID: derivedBatchID("single", operation.ID), Kind: "single", Operations: []silverOperation{operation}}, fields)
}

func decodeSettlementGroup(fields map[string]any) (settlementBatch, error) {
	operation, err := decodeSilverOperation(fields, true, true)
	if err != nil {
		return settlementBatch{}, err
	}
	return boundedBatch(settlementBatch{ID: derivedBatchID("group", operation.ID), Kind: "group", Operations: []silverOperation{operation}}, fields)
}

func derivedBatchID(kind, id string) string {
	digest := sha256.Sum256([]byte(id))
	return kind + "-" + hex.EncodeToString(digest[:])
}

func boundedBatch(batch settlementBatch, request map[string]any) (settlementBatch, error) {
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > maxSettlementBytes {
		return batch, errInvalidSettlement
	}
	return batch, nil
}

func decodeSilverOperation(fields map[string]any, group, versioned bool) (silverOperation, error) {
	names := []string{"operation_id", "business", "business_ref", "detail"}
	if group {
		names = append(names, "changes")
	} else {
		names = append(names, "user_id", "debit", "credit")
	}
	if versioned {
		names = append(names, "version")
	}
	operation := silverOperation{}
	if !exactFields(fields, names...) || versioned && !validVersion(fields) {
		return operation, errInvalidSettlement
	}
	operation.ID, _ = fields["operation_id"].(string)
	operation.Business, _ = fields["business"].(string)
	operation.Reference, _ = fields["business_ref"].(string)
	if !validSettlementID(operation.ID) || !validSettlementID(operation.Business) || operation.Business == "wallet_init" || !validSettlementID(operation.Reference) {
		return operation, errInvalidSettlement
	}
	detail, ok := fields["detail"].(map[string]any)
	nodes := 0
	if !ok || !validDetail(detail, 0, &nodes) {
		return operation, errInvalidSettlement
	}
	if detail == nil {
		detail = map[string]any{}
	}
	operation.Detail, _ = json.Marshal(detail)
	if len(operation.Detail) > 64*1024 {
		return operation, errInvalidSettlement
	}
	if group {
		values, ok := fields["changes"].([]any)
		if !ok || len(values) < 2 || len(values) > maxSettlementParticipants {
			return operation, errInvalidSettlement
		}
		for _, value := range values {
			change, ok := value.(map[string]any)
			if !ok || !exactFields(change, "user_id", "debit", "credit") {
				return operation, errInvalidSettlement
			}
			decoded, err := decodeSilverChange(change)
			if err != nil {
				return operation, err
			}
			operation.Changes = append(operation.Changes, decoded)
		}
	} else {
		change, err := decodeSilverChange(fields)
		if err != nil {
			return operation, err
		}
		operation.Changes = []silverChange{change}
	}
	sort.Slice(operation.Changes, func(i, j int) bool { return operation.Changes[i].UserID < operation.Changes[j].UserID })
	var debit, credit int64
	for i, change := range operation.Changes {
		if i > 0 && change.UserID == operation.Changes[i-1].UserID || debit > math.MaxInt64-change.Debit || credit > math.MaxInt64-change.Credit {
			return operation, errInvalidSettlement
		}
		debit += change.Debit
		credit += change.Credit
	}
	return operation, nil
}

func decodeSilverChange(fields map[string]any) (silverChange, error) {
	id, idOK := fields["user_id"].(uint64)
	debit, debitOK := fields["debit"].(int64)
	credit, creditOK := fields["credit"].(int64)
	if !idOK || id == 0 || !debitOK || !creditOK || debit < 0 || credit < 0 {
		return silverChange{}, errInvalidSettlement
	}
	return silverChange{UserID: id, Debit: debit, Credit: credit}, nil
}

// validDetail 仅校验有界的通用归档数据，不解释玩法字段；金额只允许整数。
func validDetail(value any, depth int, nodes *int) bool {
	*nodes += 1
	if depth > 8 || *nodes > 2048 {
		return false
	}
	switch entry := value.(type) {
	case nil, bool, int64, uint64:
		return true
	case string:
		return len(entry) <= 64*1024 && utf8.ValidString(entry)
	case []any:
		for _, item := range entry {
			if !validDetail(item, depth+1, nodes) {
				return false
			}
		}
		return true
	case map[string]any:
		for key, item := range entry {
			if len(key) > 128 || !utf8.ValidString(key) || !validDetail(item, depth+1, nodes) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// applySilverOperation 先验证全部参与者再修改工作副本，业务拒绝不产生部分余额变化。
func applySilverOperation(wallets map[uint64]walletBalance, operation silverOperation) string {
	for _, change := range operation.Changes {
		balance := wallets[change.UserID]
		if balance.Amount < change.Debit {
			return "insufficient_silver"
		}
		if balance.Amount-change.Debit > math.MaxInt64-change.Credit {
			return "balance_overflow"
		}
		if balance.Version == math.MaxUint64 {
			return "version_overflow"
		}
	}
	for _, change := range operation.Changes {
		balance := wallets[change.UserID]
		balance.Amount = balance.Amount - change.Debit + change.Credit
		balance.Version++
		wallets[change.UserID] = balance
	}
	return ""
}
