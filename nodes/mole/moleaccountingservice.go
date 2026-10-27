package main

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
)

const (
	moleAccountingName                            = "mole-accounting"
	accountingLoadCommand       runtime.CommandID = 1
	accountingStatusCommand     runtime.CommandID = 2
	accountingSetBalanceCommand runtime.CommandID = 3
	accountingPoolCommand       runtime.CommandID = 4
	accountingInitPoolCommand   runtime.CommandID = 5
	accountingResumeCommand     runtime.CommandID = 6
	accountingHitCommand        runtime.CommandID = 10
	accountingTickCommand       runtime.CommandID = 11
	accountingForgetCommand     runtime.CommandID = 12
	accountingSettledCommand    runtime.CommandID = 13
	accountingPeriod                              = 100 * time.Millisecond
	maxBatchOperations                            = 500
	maxBatchBytes                                 = 256 * 1024
	maxPendingHits                                = 4096
	maxCompletedHits                              = 4096
)

type silverBalance struct {
	Amount  int64
	Version uint64
}

func (balance silverBalance) snapshot() map[string]any {
	return map[string]any{"amount": balance.Amount, "version": balance.Version}
}

type moleBatch struct {
	ID     string
	Events []hitEvent
}

func (batch moleBatch) clone() moleBatch {
	return moleBatch{ID: batch.ID, Events: append([]hitEvent(nil), batch.Events...)}
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

// memoryHitResult 是本地 Call 的独立值快照，不代表持久化完成。
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

// moleAccount 将同一用户的确认余额、内存修订与退出标记放在一起，仅归 Accounting 修改。
type moleAccount struct {
	Balance  silverBalance
	Revision uint64
	Departed bool
}

// moleAccountingService 只拥有内存账；不得执行 SQL、远端 Call 或等待提交 Service。
type moleAccountingService struct {
	accounts       map[uint64]*moleAccount
	pending        map[string]accountingEntry
	completed      map[string]accountingEntry
	completedOrder []string
	buffer         []hitEvent
	bufferBytes    int
	inFlightID     string
	pool           molePool
	ready          bool
	dispatcher     *gsr.Dispatcher
}

var _ gsr.Service = (*moleAccountingService)(nil)

func newMoleAccountingService() *moleAccountingService {
	service := &moleAccountingService{
		accounts: make(map[uint64]*moleAccount), pending: make(map[string]accountingEntry),
		completed: make(map[string]accountingEntry),
	}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		accountingLoadCommand: service.loadBalance, accountingStatusCommand: service.operationStatus,
		accountingSetBalanceCommand: service.setBalance, accountingPoolCommand: service.poolSnapshot,
		accountingInitPoolCommand: service.initPool, accountingResumeCommand: service.resumeBalance,
		accountingHitCommand:  service.hit,
		accountingTickCommand: service.tick, accountingForgetCommand: service.forgetBalance,
		accountingSettledCommand: service.settled,
	})
	return service
}

func (service *moleAccountingService) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(accountingPeriod, accountingTickCommand)
	return err
}

func (service *moleAccountingService) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleAccountingService) OnStop(_ context.Context) error {
	// 所有未获持久确认的本地操作均为易失内存，不承诺进程崩溃后补交。
	return nil
}

func (service *moleAccountingService) initPool(_ gsr.CommandContext, pool molePool) error {
	if !service.ready {
		service.pool, service.ready = pool, true
	}
	return nil
}

func (service *moleAccountingService) poolSnapshot(_ gsr.CommandContext) (map[string]any, error) {
	if !service.ready {
		return nil, errAccountingUnavailable
	}
	return service.pool.snapshot(), nil
}

func (service *moleAccountingService) loadBalance(_ gsr.CommandContext, userID uint64) (map[string]any, error) {
	return service.balanceView(userID)
}

// resumeBalance 进入时复用本地账并取消延迟回收；真正的缓存缺失返回 nil，错误不当作未命中。
func (service *moleAccountingService) resumeBalance(_ gsr.CommandContext, userID uint64) (any, error) {
	if userID == 0 {
		return nil, errInvalidMoleInput
	}
	if !service.ready {
		return nil, errAccountingUnavailable
	}
	account := service.accounts[userID]
	if account == nil {
		return nil, nil
	}
	account.Departed = false
	return service.balanceView(userID)
}

func (service *moleAccountingService) setBalance(_ gsr.CommandContext, userID uint64, value map[string]any) (map[string]any, error) {
	if userID == 0 {
		return nil, errInvalidMoleInput
	}
	if !service.ready {
		return nil, errAccountingUnavailable
	}
	if service.accounts[userID] == nil && len(service.accounts) >= maxMoleUsers {
		return nil, errMoleBusy
	}
	balance, err := decodeBalance(value)
	if err != nil {
		return nil, err
	}
	account := service.accounts[userID]
	// 普通余额不含已覆盖的 HitID；查询期间新增了待确认操作时也不能重设确认基线。
	if account == nil || !service.hasPending(userID) {
		account = service.updateBalance(userID, balance)
	}
	account.Departed = false
	return service.balanceView(userID)
}

func (service *moleAccountingService) updateBalance(userID uint64, balance silverBalance) *moleAccount {
	account := service.accounts[userID]
	if account == nil {
		account = &moleAccount{Balance: balance, Revision: 1}
		service.accounts[userID] = account
	} else if balance.Version > account.Balance.Version {
		account.Balance = balance
		account.Revision++
	}
	return account
}

// balanceAmounts 一次遍历计算显示余额和可用额，不新增需要同步维护的累计状态。
func (service *moleAccountingService) balanceAmounts(userID uint64) (int64, int64, error) {
	account := service.accounts[userID]
	if !service.ready || account == nil {
		return 0, 0, errAccountingUnavailable
	}
	var debit, delta int64
	for _, entry := range service.pending {
		event := entry.Result.Event
		if event.UserID == userID {
			debit += event.Debit
			delta += event.Credit - event.Debit
		}
	}
	amount := account.Balance.Amount
	if delta > 0 && amount > math.MaxInt64-delta {
		return 0, 0, errInvalidMoleInput
	}
	return amount + delta, max(amount-debit, 0), nil
}

func (service *moleAccountingService) balanceView(userID uint64) (map[string]any, error) {
	amount, available, err := service.balanceAmounts(userID)
	if err != nil {
		return nil, err
	}
	account := service.accounts[userID]
	return map[string]any{"balance": amount, "confirmed_balance": account.Balance.Amount, "version": account.Balance.Version,
		"available": available, "revision": account.Revision}, nil
}

func (service *moleAccountingService) accept(event hitEvent, retry bool) (memoryHitResult, error) {
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
	// 丢回复后的重试不能在缓存缺失时重新扣款，需按原 HitID 查询 settlement。
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
	service.buffer = append(service.buffer, event)
	service.bufferBytes += len(encoded) + 1
	return result, nil
}

func (service *moleAccountingService) hit(self gsr.CommandContext, event hitEvent, retry bool) (memoryHitResult, error) {
	result, err := service.accept(event, retry)
	if err != nil {
		return memoryHitResult{}, err
	}
	if len(service.buffer) >= maxBatchOperations || service.bufferBytes >= maxBatchBytes-128 {
		if err := service.flush(self); err != nil {
			logMoleError(self, "提交队列未接纳，保留内存操作", err)
		}
	}
	return result, nil
}

func (service *moleAccountingService) flush(self gsr.ServiceContext) error {
	if service.inFlightID != "" || len(service.buffer) == 0 {
		return nil
	}
	id, err := randomInstance("mole-batch-")
	if err != nil {
		return err
	}
	count := min(len(service.buffer), maxBatchOperations)
	batch := moleBatch{ID: id, Events: append([]hitEvent(nil), service.buffer[:count]...)}
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
	service.inFlightID = batch.ID
	service.buffer = service.buffer[count:]
	service.bufferBytes = 0
	if len(service.buffer) > 0 {
		service.bufferBytes = batchSize(moleBatch{Events: service.buffer})
	}
	return nil
}

func (service *moleAccountingService) tick(self gsr.CommandContext) error {
	err := service.flush(self)
	if err != nil {
		logMoleError(self, "地鼠归批未完成，保留内存操作", err)
	}
	if _, timerErr := self.After(accountingPeriod, accountingTickCommand); timerErr != nil {
		service.ready = false
		return timerErr
	}
	return err
}

func (service *moleAccountingService) settled(self gsr.CommandContext, receipt batchReceipt) error {
	if service.inFlightID == "" || service.inFlightID != receipt.BatchID {
		return nil
	}
	before := make(map[uint64]int64)
	for userID := range receipt.Balances {
		before[userID], _, _ = service.balanceAmounts(userID)
	}
	service.applyReceipt(receipt)
	service.inFlightID = ""
	for userID := range receipt.Balances {
		view, err := service.balanceView(userID)
		if err == nil && view["balance"] != before[userID] {
			if err := self.SendName(moleServiceName, moleBalanceCommand, userID, view); err != nil {
				logMoleError(self, "余额校正未接纳，重连可刷新", err)
			}
		}
		service.evictIfIdle(userID)
	}
	return service.flush(self)
}

func (service *moleAccountingService) applyReceipt(receipt batchReceipt) {
	for userID, balance := range receipt.Balances {
		service.updateBalance(userID, balance)
	}
	for _, result := range receipt.Results {
		entry, exists := service.pending[result.HitID]
		if !exists {
			continue
		}
		delete(service.pending, result.HitID)
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

func (service *moleAccountingService) operationStatus(_ gsr.CommandContext, userID uint64, hitID string) (any, error) {
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

func (service *moleAccountingService) forgetBalance(_ gsr.CommandContext, userID uint64) error {
	if account := service.accounts[userID]; account != nil {
		account.Departed = true
		service.evictIfIdle(userID)
	}
	return nil
}

func (service *moleAccountingService) evictIfIdle(userID uint64) {
	account := service.accounts[userID]
	if account == nil || !account.Departed {
		return
	}
	if !service.hasPending(userID) {
		delete(service.accounts, userID)
	}
}

func (service *moleAccountingService) hasPending(userID uint64) bool {
	for _, entry := range service.pending {
		if entry.Result.Event.UserID == userID {
			return true
		}
	}
	return false
}

func validHitEvent(event hitEvent) bool {
	return event.UserID != 0 && validMoleIdentifier(event.HitID) && validMoleIdentifier(event.AttackName) &&
		validMoleIdentifier(event.ActionID) && event.Sequence != 0 && event.Hole >= 0 && event.Hole < attackHoles &&
		event.Rule == moleRuleVersion && event.Debit == hammerCost && (event.Credit == 0 || event.Credit == hammerReward)
}
