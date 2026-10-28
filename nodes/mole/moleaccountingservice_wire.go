package main

import (
	"encoding/json"
	"fmt"
)

func (batch moleBatch) wire() map[string]any {
	operations := make([]any, 0, len(batch.Events))
	for _, event := range batch.Events {
		operations = append(operations, event.operationWire())
	}
	return map[string]any{"version": uint64(1), "batch_id": batch.ID, "operations": operations}
}

func (event hitEvent) operationWire() map[string]any {
	return map[string]any{
		"operation_id": event.HitID, "business": "mole",
		"business_ref": fmt.Sprintf("%s/%d/%d", event.AttackName, event.Sequence, event.Hole),
		"user_id":      event.UserID, "debit": event.Debit, "credit": event.Credit,
		"detail": map[string]any{"rule": event.Rule, "action_id": event.ActionID},
	}
}

func batchSize(batch moleBatch) int {
	contents, err := json.Marshal(batch.wire())
	if err != nil {
		return maxBatchBytes + 1
	}
	return len(contents)
}

func decodeBalance(value any) (silverBalance, error) {
	fields, ok := value.(map[string]any)
	if !ok {
		return silverBalance{}, errAccountingUnavailable
	}
	amount, amountOK := fields["amount"].(int64)
	version, versionOK := fields["version"].(uint64)
	if !amountOK || !versionOK || amount < 0 {
		return silverBalance{}, errAccountingUnavailable
	}
	return silverBalance{Amount: amount, Version: version}, nil
}

func decodePool(value any) (molePool, error) {
	fields, ok := value.(map[string]any)
	if !ok {
		return molePool{}, errAccountingUnavailable
	}
	hits, hitsOK := fields["operation_count"].(int64)
	debit, debitOK := fields["debit"].(int64)
	credit, creditOK := fields["credit"].(int64)
	if !hitsOK || !debitOK || !creditOK || hits < 0 || debit < 0 || credit < 0 {
		return molePool{}, errAccountingUnavailable
	}
	return molePool{Hits: hits, Debit: debit, Credit: credit}, nil
}

func decodeReceipt(value any, batch moleBatch) (batchReceipt, error) {
	receipt := batchReceipt{BatchID: batch.ID, Balances: make(map[uint64]silverBalance)}
	fields, ok := value.(map[string]any)
	if !ok || fields["batch_id"] != batch.ID {
		return receipt, errAccountingUnavailable
	}
	results, resultsOK := fields["results"].([]any)
	balances, balancesOK := fields["balances"].([]any)
	if !resultsOK || !balancesOK || len(results) != len(batch.Events) {
		return receipt, errAccountingUnavailable
	}
	expected := make(map[string]hitEvent, len(batch.Events))
	users := make(map[uint64]bool)
	for _, event := range batch.Events {
		expected[event.HitID] = event
		users[event.UserID] = true
	}
	for _, value := range results {
		result, ok := value.(map[string]any)
		if !ok {
			return receipt, errAccountingUnavailable
		}
		id, idOK := result["operation_id"].(string)
		status, statusOK := result["status"].(string)
		origin, originOK := result["origin_batch"].(string)
		if _, exists := expected[id]; !exists || !idOK || !statusOK || !originOK || !validMoleIdentifier(origin) ||
			(status != "committed" && status != "rejected") {
			return receipt, errAccountingUnavailable
		}
		delete(expected, id)
		receipt.Results = append(receipt.Results, operationResult{HitID: id, Status: status, OriginBatch: origin})
	}
	for _, value := range balances {
		fields, ok := value.(map[string]any)
		if !ok {
			return receipt, errAccountingUnavailable
		}
		userID, ok := fields["user_id"].(uint64)
		if !ok || !users[userID] {
			return receipt, errAccountingUnavailable
		}
		delete(users, userID)
		balance, err := decodeBalance(fields)
		if err != nil {
			return receipt, err
		}
		receipt.Balances[userID] = balance
	}
	if len(users) != 0 {
		return receipt, errAccountingUnavailable
	}
	return receipt, nil
}
