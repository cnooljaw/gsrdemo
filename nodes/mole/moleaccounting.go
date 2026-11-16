package main

import (
	"encoding/json"
	"math"
	"time"

	"github.com/cnooljaw/gsr"
)

const (
	accountingPeriod   = 100 * time.Millisecond
	maxBatchOperations = 500
	maxBatchBytes      = 256 * 1024
	maxPendingHits     = 4096
	maxCompletedHits   = 4096
)

type silverBalance struct {
	Amount  int64
	Version uint64
}

func (balance silverBalance) snapshot() map[string]any {
	return map[string]any{"amount": balance.Amount, "version": balance.Version}
}

type moleBatch struct {
	ID       string     `json:"id"`
	Sequence uint64     `json:"sequence"`
	Events   []hitEvent `json:"events"`
}

func (batch moleBatch) clone() moleBatch {
	return moleBatch{ID: batch.ID, Sequence: batch.Sequence, Events: append([]hitEvent(nil), batch.Events...)}
}

type operationResult struct {
	HitID       string
	Status      string
	OriginBatch string
}

type batchReceipt struct {
	BatchID  string
	Results  []operationResult
	Balances map[uint64]silverBalance
}

func (receipt batchReceipt) clone() batchReceipt {
	copied := batchReceipt{BatchID: receipt.BatchID, Results: append([]operationResult(nil), receipt.Results...),
		Balances: make(map[uint64]silverBalance, len(receipt.Balances))}
	for userID, balance := range receipt.Balances {
		copied.Balances[userID] = balance
	}
	return copied
}

// memoryHitResult 是内存命中的独立值快照，不代表持久化完成。
type memoryHitResult struct {
	Event     hitEvent
	Balance   int64
	Available int64
	Revision  uint64
}

func (result memoryHitResult) snapshot() map[string]any {
	view := result.Event.snapshot("accepted")
	view["balance"], view["available"], view["revision"] = result.Balance, result.Available, result.Revision
	return view
}

type accountingEntry struct {
	Result memoryHitResult
	Status string
}

// moleAccount 保存用户确认余额和待扣索引，仅由 MoleService 修改。
type moleAccount struct {
	Balance  silverBalance
	Revision uint64
	// Pending 只索引本人的待确认 HitID，内容仍由全局幂等索引唯一持有。
	Pending map[string]struct{}
}

// moleAccounting 是 MoleService 私有的内存账，不拥有邮箱或生命周期，不执行阻塞 I/O。
type moleAccounting struct {
	accounts       map[uint64]*moleAccount
	pending        map[string]accountingEntry
	completed      map[string]accountingEntry
	completedOrder []string
	buffer         []hitEvent
	bufferBytes    int
	inFlightID     string
	pool           molePool
	sequence       uint64
	ready          bool
}

func newMoleAccounting() moleAccounting {
	return moleAccounting{accounts: make(map[uint64]*moleAccount), pending: make(map[string]accountingEntry), completed: make(map[string]accountingEntry)}
}

func (service *moleAccounting) initPool(pool molePool, sequence uint64) {
	if !service.ready {
		service.pool, service.sequence, service.ready = pool, sequence, true
	}
}

func (service *moleAccounting) updateBalance(userID uint64, balance silverBalance) *moleAccount {
	account := service.accounts[userID]
	if account == nil {
		account = &moleAccount{Balance: balance, Revision: 1, Pending: make(map[string]struct{})}
		service.accounts[userID] = account
	} else if balance.Version > account.Balance.Version {
		account.Balance = balance
		account.Revision++
	}
	return account
}

// balanceAmounts 只遍历本人的待确认操作，其他用户积压不增加本次余额计算量。
func (service *moleAccounting) balanceAmounts(userID uint64) (int64, int64, error) {
	account := service.accounts[userID]
	if !service.ready || account == nil {
		return 0, 0, errAccountingUnavailable
	}
	var debit, delta int64
	for hitID := range account.Pending {
		event := service.pending[hitID].Result.Event
		debit += event.Debit
		delta += event.Credit - event.Debit
	}
	amount := account.Balance.Amount
	if delta > 0 && amount > math.MaxInt64-delta {
		return 0, 0, errInvalidMoleInput
	}
	// 确认基线可能因外部扣款降为零；后续待操作仍要保留，但显示不能透支。
	return max(amount+delta, 0), max(amount-debit, 0), nil
}

func (service *moleAccounting) balanceSnapshot(userID uint64) (map[string]any, error) {
	amount, available, err := service.balanceAmounts(userID)
	if err != nil {
		return nil, err
	}
	account := service.accounts[userID]
	return map[string]any{"balance": amount, "confirmed_balance": account.Balance.Amount, "version": account.Balance.Version,
		"available": available, "revision": account.Revision}, nil
}

func (service *moleAccounting) accept(event hitEvent, retry bool) (memoryHitResult, error) {
	if !validHitEvent(event) {
		return memoryHitResult{}, errInvalidMoleInput
	}
	for _, entries := range []map[string]accountingEntry{service.pending, service.completed} {
		if previous, exists := entries[event.HitID]; exists {
			if previous.Result.Event != event {
				return memoryHitResult{}, errMoleConflict
			}
			return previous.Result, nil
		}
	}
	// 丢回复后的重试不能在缓存缺失时重新扣款；原 HitID 留给受控验收或管理工具核查。
	if retry {
		return memoryHitResult{}, errMoleOperationUnknown
	}
	account := service.accounts[event.UserID]
	if !service.ready || account == nil {
		return memoryHitResult{}, errAccountingUnavailable
	}
	if len(service.pending) >= maxPendingHits {
		return memoryHitResult{}, errMoleBusy
	}
	amount, available, err := service.balanceAmounts(event.UserID)
	if err != nil {
		return memoryHitResult{}, err
	}
	if available < event.Debit {
		return memoryHitResult{}, errInsufficientSilver
	}
	if amount > math.MaxInt64-max(event.Credit-event.Debit, 0) {
		return memoryHitResult{}, errInvalidMoleInput
	}
	encoded, err := json.Marshal(event.operationWire())
	if err != nil {
		return memoryHitResult{}, err
	}
	account.Revision++
	result := memoryHitResult{Event: event, Balance: amount - event.Debit + event.Credit,
		Available: available - event.Debit, Revision: account.Revision}
	service.pending[event.HitID] = accountingEntry{Result: result, Status: "accepted"}
	account.Pending[event.HitID] = struct{}{}
	service.buffer = append(service.buffer, event)
	service.bufferBytes += len(encoded) + 1
	return result, nil
}

func (service *moleAccounting) flush(self gsr.ServiceContext) error {
	if !service.ready || service.inFlightID != "" || len(service.buffer) == 0 {
		return nil
	}
	if service.sequence == math.MaxUint64 {
		return errMoleBusy
	}
	id, err := randomInstance("mole-batch-")
	if err != nil {
		return err
	}
	count := min(len(service.buffer), maxBatchOperations)
	batch := moleBatch{ID: id, Sequence: service.sequence + 1, Events: append([]hitEvent(nil), service.buffer[:count]...)}
	for batchSize(batch) > maxBatchBytes && count > 1 {
		count /= 2
		batch.Events = batch.Events[:count]
	}
	if batchSize(batch) > maxBatchBytes {
		return errInvalidMoleInput
	}
	if err := self.SendName(moleSubmissionName, submissionBatchCommand, batch.clone()); err != nil {
		return err
	}
	service.inFlightID, service.sequence = batch.ID, batch.Sequence
	service.buffer = service.buffer[count:]
	service.bufferBytes = 0
	if len(service.buffer) > 0 {
		service.bufferBytes = batchSize(moleBatch{Events: service.buffer})
	}
	return nil
}

func (service *moleAccounting) applyReceipt(receipt batchReceipt) {
	for userID, balance := range receipt.Balances {
		service.updateBalance(userID, balance)
	}
	for _, result := range receipt.Results {
		entry, exists := service.pending[result.HitID]
		if !exists {
			continue
		}
		delete(service.pending, result.HitID)
		delete(service.accounts[entry.Result.Event.UserID].Pending, result.HitID)
		entry.Status = result.Status
		service.completed[result.HitID] = entry
		service.completedOrder = append(service.completedOrder, result.HitID)
		if result.Status == "committed" && result.OriginBatch == receipt.BatchID {
			if err := service.pool.add(entry.Result.Event.Debit, entry.Result.Event.Credit); err != nil {
				service.ready = false
			}
		}
	}
	for len(service.completedOrder) > maxCompletedHits {
		delete(service.completed, service.completedOrder[0])
		service.completedOrder = service.completedOrder[1:]
	}
	for userID := range receipt.Balances {
		service.accounts[userID].Revision++
	}
}

func (service *moleAccounting) operationStatus(userID uint64, hitID string) (any, error) {
	if userID == 0 || !validMoleIdentifier(hitID) {
		return nil, errInvalidMoleInput
	}
	for _, entries := range []map[string]accountingEntry{service.pending, service.completed} {
		if entry, exists := entries[hitID]; exists {
			if entry.Result.Event.UserID != userID {
				return nil, errInvalidMoleInput
			}
			return entry.Result.Event.snapshot(entry.Status), nil
		}
	}
	return nil, nil
}

func (service *moleAccounting) hasPending(userID uint64) bool {
	account := service.accounts[userID]
	return account != nil && len(account.Pending) != 0
}

func validHitEvent(event hitEvent) bool {
	return event.UserID != 0 && validMoleIdentifier(event.HitID) && validMoleIdentifier(event.AttackName) &&
		validMoleIdentifier(event.ActionID) && event.Sequence != 0 && event.Hole >= 0 && event.Hole < attackHoles &&
		event.Rule == moleRuleVersion && event.Debit == hammerCost && (event.Credit == 0 || event.Credit == hammerReward)
}
