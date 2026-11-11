// 共享 TCP 后端与节点夹具；只提供被测节点依赖的契约，不替代被测 Service。
package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
	"github.com/cnooljaw/gsrdemo/common/rpc/moleattack"
	"github.com/cnooljaw/gsrdemo/common/rpc/moleentry"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementread"
	"github.com/cnooljaw/gsrdemo/common/rpc/settlementwrite"
)

const moleTestWait = 8 * time.Second

// settlementFixture 仅实现 mole 用到的单用户测试契约，不是 settlement 业务实现。
type settlementFixture struct {
	balances        map[uint64]silverBalance
	batches         map[string]map[string]any
	operations      map[string]map[string]any
	checkpoint      moleCheckpoint
	firstReplyDelay time.Duration
	firstError      error
	badFirstReceipt bool
	hideReceipts    bool
	submitted       chan int
	submitRequests  chan map[string]any
	submitCount     atomic.Uint64
	batchQueries    chan struct{}
	balanceRequests chan uint64
	rejectUser      uint64
	dispatcher      *gsr.Dispatcher
}

var _ gsr.Service = (*settlementFixture)(nil)

func newSettlementFixture() *settlementFixture {
	service := &settlementFixture{balances: make(map[uint64]silverBalance), batches: make(map[string]map[string]any),
		operations: make(map[string]map[string]any), submitted: make(chan int, 32),
		submitRequests: make(chan map[string]any, 2), batchQueries: make(chan struct{}, 1), balanceRequests: make(chan uint64, 2)}
	for userID := uint64(1); userID <= 20; userID++ {
		service.balances[userID] = silverBalance{Amount: 10000, Version: 1}
	}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		settlementwrite.SettleBatchCommand: service.handleSubmit,
		settlementread.GetBatchCommand:     service.handleBatch,
		settlementread.GetBalanceCommand:   service.handleBalance,
		settlementread.GetOperationCommand: service.handleOperation,
		998:                                service.handleChangeBalance,
		997:                                service.handleHide,
	})
	return service
}

func (service *settlementFixture) OnStart(context.Context, gsr.ServiceContext) error {
	return nil
}

func (service *settlementFixture) OnStop(context.Context) error {
	return nil
}

func (service *settlementFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *settlementFixture) handleSubmit(_ gsr.CommandContext, request map[string]any) (map[string]any, error) {
	service.submitCount.Add(1)
	// 只取前两个提交样本，持续重试时不能让测试观测阻塞结算。
	select {
	case service.submitRequests <- request:
	default:
	}
	if service.firstError != nil {
		err := service.firstError
		service.firstError = nil
		return nil, err
	}
	id := request["batch_id"].(string)
	for _, item := range request["operations"].([]any) {
		if item.(map[string]any)["user_id"] == service.rejectUser {
			return nil, runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.identity_conflict", "injected permanent conflict")
		}
	}
	if previous, exists := service.batches[id]; exists {
		if service.hideReceipts {
			return nil, runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.store_unavailable", "injected unknown SQL result")
		}
		return previous, nil
	}
	operations := request["operations"].([]any)
	results := make([]any, 0, len(operations))
	users := make(map[uint64]bool)
	for _, value := range operations {
		op := value.(map[string]any)
		operationID := op["operation_id"].(string)
		change := op
		userID := change["user_id"].(uint64)
		users[userID] = true
		if previous, exists := service.operations[operationID]; exists {
			results = append(results, previous)
			continue
		}
		var debit, credit int64
		for _, value := range change["items"].([]any) {
			item := value.(map[string]any)
			debit += item["debit"].(int64)
			credit += item["credit"].(int64)
		}
		balance := service.balances[userID]
		status := "rejected"
		if balance.Amount >= debit {
			balance.Amount += credit - debit
			balance.Version++
			service.balances[userID] = balance
			status = "committed"
		}
		result := map[string]any{"operation_id": operationID, "status": status, "origin_batch": id}
		service.operations[operationID] = result
		results = append(results, result)
	}
	balances := make([]any, 0, len(users))
	for userID := range users {
		balance := service.balances[userID].snapshot()
		balance["user_id"] = userID
		balances = append(balances, balance)
	}
	receipt := map[string]any{"batch_id": id, "results": results, "balances": balances}
	service.batches[id] = receipt
	service.submitted <- len(operations)
	if service.hideReceipts {
		return nil, runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.store_unavailable", "injected unknown SQL result")
	}
	if service.firstReplyDelay != 0 {
		delay := service.firstReplyDelay
		service.firstReplyDelay = 0
		// SQL 已提交但本次 Call 等待超时，迟到返回不改变原批次的幂等结果。
		time.Sleep(delay)
	}
	if service.badFirstReceipt {
		service.badFirstReceipt = false
		return map[string]any{"batch_id": id}, nil
	}
	return receipt, nil
}

func (service *settlementFixture) handleBatch(_ gsr.CommandContext, id string) (any, error) {
	select {
	case service.batchQueries <- struct{}{}:
	default:
	}
	if service.hideReceipts {
		return nil, nil
	}
	if batch, exists := service.batches[id]; exists {
		return batch, nil
	}
	return nil, nil
}

func (service *settlementFixture) handleHide(_ gsr.CommandContext, hidden bool) error {
	service.hideReceipts = hidden
	return nil
}

func (service *settlementFixture) handleBalance(_ gsr.CommandContext, userID uint64) (map[string]any, error) {
	select {
	case service.balanceRequests <- userID:
	default:
	}
	return service.balances[userID].snapshot(), nil
}

func (service *settlementFixture) handleOperation(_ gsr.CommandContext, _ uint64, id string) (any, error) {
	if result, exists := service.operations[id]; exists {
		return result, nil
	}
	return nil, nil
}

func (service *settlementFixture) handleChangeBalance(_ gsr.CommandContext, userID uint64, amount int64) error {
	balance := service.balances[userID]
	balance.Amount = amount
	balance.Version++
	service.balances[userID] = balance
	return nil
}

type onlineFixture struct {
	locations  map[uint64]map[string]any
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*onlineFixture)(nil)

func newOnlineFixture() *onlineFixture {
	service := &onlineFixture{locations: make(map[uint64]map[string]any)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		onlinerpc.BindCommand:   service.handleBind,
		onlinerpc.LookupCommand: service.handleLookup,
		99:                      service.handleHalt,
	})
	return service
}

func (service *onlineFixture) OnStart(context.Context, gsr.ServiceContext) error {
	return nil
}

func (service *onlineFixture) OnStop(context.Context) error {
	return nil
}

func (service *onlineFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *onlineFixture) handleBind(_ gsr.CommandContext, userID uint64, loginID, access, agent string) error {
	service.locations[userID] = map[string]any{"login_id": loginID, "access_node": access, "agent_name": agent}
	return nil
}

func (service *onlineFixture) handleLookup(_ gsr.CommandContext, userID uint64) (any, error) {
	if location, exists := service.locations[userID]; exists {
		return location, nil
	}
	return nil, nil
}

func (service *onlineFixture) handleHalt(self gsr.CommandContext) error {
	self.Exit()
	return nil
}

type moleTestPeer struct {
	node    string
	name    string
	service gsr.Service
}

var _ gsr.Service = (*moleTestPeer)(nil)

func (service *moleTestPeer) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	id, err := self.CreateService(ctx, service.service)
	if err != nil {
		return err
	}
	if err := self.Node().RegisterService(cluster.ServiceName(service.name), id); err != nil {
		return err
	}
	if service.name == "settlement" {
		for _, name := range []string{settlementwrite.ServiceName, settlementread.ServiceName} {
			if err := self.Node().RegisterService(cluster.ServiceName(name), id); err != nil {
				return err
			}
		}
	}
	if err := self.Node().OpenCluster(cluster.NodeID(service.node)); err != nil {
		return err
	}
	self.Exit()
	return nil
}

func (service *moleTestPeer) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (service *moleTestPeer) OnStop(context.Context) error {
	return nil
}

type moleObservedMain struct {
	service gsr.Service
	ready   chan string
}

var _ gsr.Service = (*moleObservedMain)(nil)

func (service *moleObservedMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	if err := service.service.OnStart(ctx, self); err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(self.Node().ClusterListenAddress())
	if err != nil {
		return err
	}
	service.ready <- net.JoinHostPort("127.0.0.1", port)
	return nil
}

func (service *moleObservedMain) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.service.Handle(self, command, args...)
}

func (service *moleObservedMain) OnStop(ctx context.Context) error {
	return service.service.OnStop(ctx)
}

func startMoleTestNode(t *testing.T, node string, routes map[cluster.NodeID]string, main gsr.Service) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	observed := &moleObservedMain{service: main, ready: make(chan string, 1)}
	directory, done := t.TempDir(), make(chan error, 1)
	copyRoutes := make(map[cluster.NodeID]string)
	for name, address := range routes {
		copyRoutes[name] = address
	}
	// Call 回复沿原连接返回，不再为结算回调提前预选本机端口。
	copyRoutes[cluster.NodeID(node)] = "127.0.0.1:0"
	go func() {
		done <- gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: directory}, Cluster: gsr.ClusterConfig{Nodes: copyRoutes}}, observed)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("%s stop: %v", node, err)
				}
			case <-time.After(3 * time.Second):
				t.Errorf("%s stop timed out", node)
			}
		})
	}
	t.Cleanup(stop)
	select {
	case address := <-observed.ready:
		return address, stop
	case err := <-done:
		done <- err
		t.Fatalf("%s start: %v", node, err)
	case <-time.After(3 * time.Second):
		t.Fatalf("%s startup timed out", node)
	}
	return "", stop
}

type moleTestCaller struct {
	work   func(gsr.ServiceContext) error
	cancel context.CancelFunc
}

var _ gsr.Service = (*moleTestCaller)(nil)

func (service *moleTestCaller) OnStart(_ context.Context, self gsr.ServiceContext) error {
	err := service.work(self)
	service.cancel()
	self.Exit()
	return err
}

func (service *moleTestCaller) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (service *moleTestCaller) OnStop(context.Context) error {
	return nil
}

func moleRPC(t *testing.T, routes map[cluster.NodeID]string, node, name string, command runtime.CommandID, args ...any) (any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var result any
	var callErr error
	caller := &moleTestCaller{cancel: cancel, work: func(self gsr.ServiceContext) error {
		wait, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		result, callErr = self.Cluster().Call(wait, cluster.NodeID(node), cluster.ServiceName(name), command, args...)
		return nil
	}}
	if err := gsr.Run(ctx, gsr.Config{Logging: logging.Config{Directory: t.TempDir()}, Cluster: gsr.ClusterConfig{Nodes: routes}}, caller); err != nil {
		t.Fatal(err)
	}
	return result, callErr
}

func newMoleTestMain(t *testing.T, fixture *settlementFixture) gsr.Service {
	t.Helper()
	if fixture.checkpoint.client == nil {
		fixture.checkpoint = testMoleCheckpoint(t)
	}
	return NewMain("mole", "settlement", fixture.checkpoint, nil)
}

func prepareMoleCluster(t *testing.T, fixture *settlementFixture) (map[cluster.NodeID]string, func()) {
	t.Helper()
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: fixture})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: onlinerpc.ServiceName, service: newOnlineFixture()})
	routes := map[cluster.NodeID]string{"online": online, "settlement": settlement}
	mole, stop := startMoleTestNode(t, "mole", routes, newMoleTestMain(t, fixture))
	routes["mole"] = mole
	return routes, stop
}

func callMoleEnter(t *testing.T, routes map[cluster.NodeID]string, args ...any) (any, error) {
	t.Helper()
	return moleRPC(t, routes, "mole", moleentry.ServiceName, moleentry.MoleEnterCommand, args...)
}

func enterMoleTestUser(t *testing.T, routes map[cluster.NodeID]string, userID uint64, login, access string) map[string]any {
	t.Helper()
	if _, err := moleRPC(t, routes, "online", onlinerpc.ServiceName, onlinerpc.BindCommand, userID, login, access, "agent"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(moleTestWait)
	for time.Now().Before(deadline) {
		value, err := callMoleEnter(t, routes, userID, login, access, "agent")
		if err == nil {
			return value.(map[string]any)
		}
		if !errors.Is(err, errAccountingUnavailable) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("mole accounting did not become ready")
	return nil
}

func testHitTarget(t *testing.T, routes map[cluster.NodeID]string, snapshot map[string]any, userID uint64, login, action string) map[string]any {
	t.Helper()
	target := snapshot["targets"].([]any)[0].(map[string]any)
	value, err := moleRPC(t, routes, "mole", snapshot["attack_name"].(string), moleattack.AttackHitCommand,
		userID, login, action, target["spawn_seq"], target["hole"])
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

func waitMoleOperation(t *testing.T, routes map[cluster.NodeID]string, userID uint64, hitID, want string) {
	t.Helper()
	deadline := time.Now().Add(moleTestWait)
	for time.Now().Before(deadline) {
		value, err := moleRPC(t, routes, "mole", moleAccountingName, accountingStatusCommand, userID, hitID)
		if err == nil && value != nil && value.(map[string]any)["status"] == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("operation %s did not reach %s", hitID, want)
}

type moleAgentMessage struct {
	LoginID string
	Command runtime.CommandID
	Payload map[string]any
}

type moleAgentFixture struct {
	messages   chan moleAgentMessage
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*moleAgentFixture)(nil)

func newMoleAgentFixture() *moleAgentFixture {
	service := &moleAgentFixture{messages: make(chan moleAgentMessage, 32)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{
		accessagent.MoleSnapshotPushCommand:          service.handleSnapshotPush,
		accessagent.MoleBalanceCorrectionPushCommand: service.handleBalanceCorrectionPush,
	})
	return service
}

func (service *moleAgentFixture) OnStart(context.Context, gsr.ServiceContext) error {
	return nil
}

func (service *moleAgentFixture) OnStop(context.Context) error {
	return nil
}

func (service *moleAgentFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.dispatcher.Handle(self, command, args...)
}

func (service *moleAgentFixture) handleSnapshotPush(_ gsr.CommandContext, loginID string, payload map[string]any) error {
	return service.recordPush(accessagent.MoleSnapshotPushCommand, loginID, payload)
}

func (service *moleAgentFixture) handleBalanceCorrectionPush(_ gsr.CommandContext, loginID string, payload map[string]any) error {
	return service.recordPush(accessagent.MoleBalanceCorrectionPushCommand, loginID, payload)
}

func (service *moleAgentFixture) recordPush(command runtime.CommandID, loginID string, payload map[string]any) error {
	select {
	case service.messages <- moleAgentMessage{LoginID: loginID, Command: command, Payload: payload}:
	default:
		return errMoleBusy
	}
	return nil
}
