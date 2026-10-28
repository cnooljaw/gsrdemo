package main

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

// settlementFixture 仅实现 mole 用到的单用户测试契约，不是 settlement 业务实现。
type settlementFixture struct {
	balances        map[uint64]silverBalance
	batches         map[string]map[string]any
	operations      map[string]map[string]any
	pool            molePool
	loseFirstReply  bool
	hideReceipts    bool
	submitted       chan int
	submitRequests  chan map[string]any
	batchQueries    chan struct{}
	balanceRequests chan uint64
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
		settlementSubmitCommand: service.submit, settlementBatchCommand: service.batch,
		settlementBalanceCommand: service.balance, settlementSummaryCommand: service.summary,
		settlementOperationCommand: service.operation, 998: service.changeBalance,
		997: service.hide,
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

func (service *settlementFixture) submit(_ gsr.CommandContext, request map[string]any) (map[string]any, error) {
	// 只取前两个提交样本，持续重试时不能让测试观测阻塞结算。
	select {
	case service.submitRequests <- request:
	default:
	}
	id := request["batch_id"].(string)
	if previous, exists := service.batches[id]; exists {
		if service.hideReceipts {
			return nil, errAccountingUnavailable
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
			if err := service.pool.add(debit, credit); err != nil {
				return nil, err
			}
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
		return nil, errAccountingUnavailable
	}
	if service.loseFirstReply {
		service.loseFirstReply = false
		// 模拟已经执行且提交，但处理回复慢于 mole 的有限 Call 预算。
		time.Sleep(650 * time.Millisecond)
	}
	return receipt, nil
}

func (service *settlementFixture) batch(_ gsr.CommandContext, id string) (any, error) {
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

func (service *settlementFixture) hide(_ gsr.CommandContext, hidden bool) error {
	service.hideReceipts = hidden
	return nil
}

func (service *settlementFixture) balance(_ gsr.CommandContext, userID uint64) (map[string]any, error) {
	select {
	case service.balanceRequests <- userID:
	default:
	}
	return service.balances[userID].snapshot(), nil
}

func (service *settlementFixture) summary(_ gsr.CommandContext, _ string) (map[string]any, error) {
	return map[string]any{"operation_count": service.pool.Hits, "debit": service.pool.Debit, "credit": service.pool.Credit}, nil
}

func (service *settlementFixture) operation(_ gsr.CommandContext, _ uint64, id string) (any, error) {
	if result, exists := service.operations[id]; exists {
		return result, nil
	}
	return nil, nil
}

func (service *settlementFixture) changeBalance(_ gsr.CommandContext, userID uint64, amount int64) error {
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
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{1: service.bind, 2: service.lookup, 99: service.halt})
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

func (service *onlineFixture) bind(_ gsr.CommandContext, userID uint64, loginID, access, agent string) error {
	service.locations[userID] = map[string]any{"login_id": loginID, "access_node": access, "agent_name": agent}
	return nil
}

func (service *onlineFixture) lookup(_ gsr.CommandContext, userID uint64) (any, error) {
	if location, exists := service.locations[userID]; exists {
		return location, nil
	}
	return nil, nil
}

func (service *onlineFixture) halt(self gsr.CommandContext) error {
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

func prepareMoleCluster(t *testing.T, fixture *settlementFixture) (map[cluster.NodeID]string, func()) {
	t.Helper()
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: fixture})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: "online", service: newOnlineFixture()})
	routes := map[cluster.NodeID]string{"online": online, "settlement": settlement}
	mole, stop := startMoleTestNode(t, "mole", routes, NewMain("mole"))
	routes["mole"] = mole
	return routes, stop
}

func enterMoleTestUser(t *testing.T, routes map[cluster.NodeID]string, userID uint64, login, access string) map[string]any {
	t.Helper()
	if _, err := moleRPC(t, routes, "online", "online", 1, userID, login, access, "agent"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		value, err := moleRPC(t, routes, "mole", moleServiceName, moleEnterCommand, userID, login, access, "agent")
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
	value, err := moleRPC(t, routes, "mole", snapshot["attack_name"].(string), attackHitCommand,
		userID, login, action, target["spawn_seq"], target["hole"])
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

func waitMoleOperation(t *testing.T, routes map[cluster.NodeID]string, userID uint64, hitID, want string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		value, err := moleRPC(t, routes, "mole", moleAccountingName, accountingStatusCommand, userID, hitID)
		if err == nil && value != nil && value.(map[string]any)["status"] == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("operation %s did not reach %s", hitID, want)
}

// TestMoleHitReturnsMemoryBalance 保护命中同步返回内存扣返和显示余额，不等待持久化确认。
func TestMoleHitReturnsMemoryBalance(t *testing.T) {
	routes, _ := prepareMoleCluster(t, newSettlementFixture())
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	want := int64(10000) - hit["debit"].(int64) + hit["credit"].(int64)
	if hit["status"] != "accepted" || hit["balance"] != want || hit["revision"] == nil {
		t.Fatalf("hit did not return a complete memory result: %v", hit)
	}
}

// TestMoleSlowSubmissionDoesNotBlockMemoryHit 保护提交 Service 正在等待慢结算时，另一组仍在本地完成记账。
func TestMoleSlowSubmissionDoesNotBlockMemoryHit(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.loseFirstReply = true
	routes, _ := prepareMoleCluster(t, fixture)
	first := enterMoleTestUser(t, routes, 1, "login-1", "access-01")
	_ = enterMoleTestUser(t, routes, 2, "login-2", "access-01")
	_ = enterMoleTestUser(t, routes, 3, "login-3", "access-02")
	other := enterMoleTestUser(t, routes, 4, "login-4", "access-02")
	_ = testHitTarget(t, routes, first, 1, "login-1", "first")
	select {
	case <-fixture.submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("submission did not reach the delayed provider")
	}
	started := time.Now()
	hit := testHitTarget(t, routes, other, 4, "login-4", "second")
	if elapsed := time.Since(started); elapsed >= 250*time.Millisecond || hit["status"] != "accepted" {
		t.Fatalf("memory hit waited for settlement: elapsed=%s result=%v", elapsed, hit)
	}
	waitMoleOperation(t, routes, 4, hit["hit_id"].(string), "committed")
}

// TestMoleSharedGroupsAndReconnect 保护真实 TCP 下分组补位、跨接入重连、迟到进入和旧连接操作隔离。
func TestMoleSharedGroupsAndReconnect(t *testing.T) {
	routes, _ := prepareMoleCluster(t, newSettlementFixture())
	first := enterMoleTestUser(t, routes, 1, "old", "access-01")
	second := enterMoleTestUser(t, routes, 2, "login-2", "access-02")
	third := enterMoleTestUser(t, routes, 3, "login-3", "access-01")
	fourth := enterMoleTestUser(t, routes, 4, "login-4", "access-02")
	name := first["attack_name"].(string)
	if second["attack_name"] != name || third["attack_name"] != name || fourth["attack_name"] == name {
		t.Fatal("three-seat grouping failed")
	}
	reconnected := enterMoleTestUser(t, routes, 1, "new", "access-02")
	if reconnected["attack_name"] != name || reconnected["seat"] != first["seat"] {
		t.Fatal("reconnect moved group or seat")
	}
	for _, command := range []runtime.CommandID{attackSnapshotCommand, attackLeaveCommand} {
		if _, err := moleRPC(t, routes, "mole", name, command, uint64(1), "old"); !errors.Is(err, errMoleStaleLogin) {
			t.Fatalf("old login command %d: %v", command, err)
		}
	}
	if _, err := moleRPC(t, routes, "mole", moleServiceName, moleEnterCommand, uint64(1), "old", "access-01", "agent"); !errors.Is(err, errMoleStaleLogin) {
		t.Fatalf("late Enter replaced new login: %v", err)
	}
	if _, err := moleRPC(t, routes, "mole", name, attackLeaveCommand, uint64(2), "login-2"); err != nil {
		t.Fatal(err)
	}
	joined := enterMoleTestUser(t, routes, 5, "login-5", "access-02")
	if joined["attack_name"] != name || joined["seat"] != second["seat"] {
		t.Fatal("vacant group or seat was not filled")
	}
}

// TestMoleLostReplyUsesOriginalBatch 保护超时后直接重提原批次，无查询前置且不重复记账。
func TestMoleLostReplyUsesOriginalBatch(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.loseFirstReply = true
	routes, _ := prepareMoleCluster(t, fixture)
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
	value, err := moleRPC(t, routes, "mole", moleAccountingName, accountingPoolCommand)
	if err != nil || value.(map[string]any)["hits"] != int64(1) {
		t.Fatalf("duplicate or missing statistics: %v %v", value, err)
	}
	if len(fixture.submitted) != 1 {
		t.Fatal("lost reply caused another financial application")
	}
	if len(fixture.batchQueries) != 0 || len(fixture.submitRequests) < 2 {
		t.Fatal("expected direct idempotent submission and retry, without GetBatch")
	}
	first, retry := <-fixture.submitRequests, <-fixture.submitRequests
	if !reflect.DeepEqual(first, retry) {
		t.Fatal("retry changed the batch identity or contents")
	}
}

// TestMoleRestartReloadsSettlementState 保护重启只重载结算端持久事实，不重放已经丢失的本地内存操作。
func TestMoleRestartReloadsSettlementState(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.loseFirstReply = true
	routes, stop := prepareMoleCluster(t, fixture)
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	select {
	case <-fixture.submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("batch was not received by settlement")
	}
	stop()
	address, _ := startMoleTestNode(t, "mole", routes, NewMain("mole"))
	routes["mole"] = address
	reconnected := enterMoleTestUser(t, routes, 1, "new-login", "access-02")
	want := int64(10000) - hit["debit"].(int64) + hit["credit"].(int64)
	if reconnected["silver"].(map[string]any)["balance"] != want {
		t.Fatal("restart did not reload settlement balance")
	}
	value, err := moleRPC(t, routes, "mole", moleSubmissionName, submissionOperationCommand, uint64(1), hit["hit_id"])
	if err != nil || value == nil || value.(map[string]any)["status"] != "committed" {
		t.Fatalf("durable operation was not queryable: %v %v", value, err)
	}
	value, err = moleRPC(t, routes, "mole", moleAccountingName, accountingPoolCommand)
	if err != nil || value.(map[string]any)["hits"] != int64(1) {
		t.Fatalf("statistics were not rebuilt: %v %v", value, err)
	}
}

// TestMoleExitDoesNotCancelHandedOffHit 保护退出回收空组后记账继续完成，重进加载确认余额且不沿用已退出组实例。
func TestMoleExitDoesNotCancelHandedOffHit(t *testing.T) {
	routes, _ := prepareMoleCluster(t, newSettlementFixture())
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	name := snapshot["attack_name"].(string)
	if _, err := moleRPC(t, routes, "mole", name, attackLeaveCommand, uint64(1), "login"); err != nil {
		t.Fatal(err)
	}
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
	reentered := enterMoleTestUser(t, routes, 1, "login", "access-01")
	if reentered["attack_name"] == name {
		t.Fatal("empty group instance was reused")
	}
	want := int64(10000) - hit["debit"].(int64) + hit["credit"].(int64)
	if reentered["silver"].(map[string]any)["balance"] != want {
		t.Fatalf("exit lost the submitted hit: %v", reentered["silver"])
	}
}

type moleAgentMessage struct {
	LoginID string
	Kind    string
	Payload map[string]any
}

type moleAgentFixture struct {
	messages   chan moleAgentMessage
	dispatcher *gsr.Dispatcher
}

var _ gsr.Service = (*moleAgentFixture)(nil)

func newMoleAgentFixture() *moleAgentFixture {
	service := &moleAgentFixture{messages: make(chan moleAgentMessage, 32)}
	service.dispatcher = gsr.MustDispatcher(map[runtime.CommandID]any{molePushCommand: service.push})
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

func (service *moleAgentFixture) push(_ gsr.CommandContext, loginID, kind string, payload map[string]any) error {
	select {
	case service.messages <- moleAgentMessage{LoginID: loginID, Kind: kind, Payload: payload}:
	default:
		return errMoleBusy
	}
	return nil
}

// TestMoleMatchingSettlementDoesNotRepeatReward 保护内存结果一致时不补发逐锤奖励，已消耗目标也不恢复。
func TestMoleMatchingSettlementDoesNotRepeatReward(t *testing.T) {
	fixture := newSettlementFixture()
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: fixture})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: "online", service: newOnlineFixture()})
	agent := newMoleAgentFixture()
	access, _ := startMoleTestNode(t, "access-01", nil, &moleTestPeer{node: "access-01", name: "agent", service: agent})
	routes := map[cluster.NodeID]string{"settlement": settlement, "online": online, "access-01": access}
	address, _ := startMoleTestNode(t, "mole", routes, NewMain("mole"))
	routes["mole"] = address
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
	for len(agent.messages) > 0 {
		message := <-agent.messages
		if message.Kind != "snapshot" {
			t.Fatalf("matching settlement emitted another result: %+v", message)
		}
	}
	current, err := moleRPC(t, routes, "mole", snapshot["attack_name"].(string), attackSnapshotCommand, uint64(1), "login")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, value := range current.(map[string]any)["hits"].([]any) {
		record := value.(map[string]any)
		if record["hit_id"] == hit["hit_id"] {
			found = true
			if record["status"] != "accepted" {
				t.Fatal("database completion changed game result")
			}
		}
	}
	if !found {
		t.Fatal("memory hit was lost")
	}
}

// TestMoleOnlineFailureOnlyBlocksEntry 保护 Online 不可用时拒绝新进入，已进入用户仍可直接命中并结算。
func TestMoleOnlineFailureOnlyBlocksEntry(t *testing.T) {
	routes, _ := prepareMoleCluster(t, newSettlementFixture())
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	if _, err := moleRPC(t, routes, "online", "online", 99); err != nil {
		t.Fatal(err)
	}
	if _, err := moleRPC(t, routes, "mole", moleServiceName, moleEnterCommand, uint64(2), "login-2", "access-02", "agent"); !errors.Is(err, errMoleOnlineUnavailable) {
		t.Fatalf("entry ignored Online failure: %v", err)
	}
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
}

// TestMoleCorrectionUsesCurrentLogin 保护结算不一致时校正当前账号余额，不作为旧命中回执推送或复活目标。
func TestMoleCorrectionUsesCurrentLogin(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.hideReceipts = true
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: fixture})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: "online", service: newOnlineFixture()})
	agent := newMoleAgentFixture()
	access, _ := startMoleTestNode(t, "access-02", nil, &moleTestPeer{node: "access-02", name: "agent", service: agent})
	routes := map[cluster.NodeID]string{"settlement": settlement, "online": online, "access-02": access}
	address, _ := startMoleTestNode(t, "mole", routes, NewMain("mole"))
	routes["mole"] = address
	snapshot := enterMoleTestUser(t, routes, 1, "old-login", "access-01")
	if _, err := moleRPC(t, routes, "settlement", "settlement", 998, uint64(1), int64(0)); err != nil {
		t.Fatal(err)
	}
	hit := testHitTarget(t, routes, snapshot, 1, "old-login", "action")
	select {
	case <-fixture.submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("hit was not submitted")
	}
	_ = enterMoleTestUser(t, routes, 1, "new-login", "access-02")
	if _, err := moleRPC(t, routes, "settlement", "settlement", 997, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case message := <-agent.messages:
			if message.Kind == "snapshot" {
				continue
			}
			if message.Kind != "balance_correction" || message.LoginID != "new-login" || message.Payload["balance"] != int64(0) {
				t.Fatalf("invalid account correction: %+v", message)
			}
			current, err := moleRPC(t, routes, "mole", snapshot["attack_name"].(string), attackSnapshotCommand, uint64(1), "new-login")
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range current.(map[string]any)["hits"].([]any) {
				record := v.(map[string]any)
				if record["hit_id"] == hit["hit_id"] && record["status"] != "accepted" {
					t.Fatal("financial correction changed hit")
				}
			}
			return
		case <-deadline:
			t.Fatal("balance was not corrected")
		}
	}
}

type moleFaultMain struct {
	main gsr.Service
}

var _ gsr.Service = (*moleFaultMain)(nil)

func (service *moleFaultMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	broken := newAttackService("attack-broken")
	// 仅测试注入非法座位，下一次加入触发真实 AttackService Handler panic。
	broken.members[20] = attackMember{UserID: 20, LoginID: "broken", Seat: 99}
	if _, err := self.CreateService(ctx, broken); err != nil {
		return err
	}
	return service.main.OnStart(ctx, self)
}

func (service *moleFaultMain) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.main.Handle(self, command, args...)
}

func (service *moleFaultMain) OnStop(ctx context.Context) error {
	return service.main.OnStop(ctx)
}

// TestMolePanickedGroupDoesNotStopOtherGroups 保护一个真实 AttackService panic 后，其他组继续响应和记账。
func TestMolePanickedGroupDoesNotStopOtherGroups(t *testing.T) {
	settlement, _ := startMoleTestNode(t, "settlement", nil, &moleTestPeer{node: "settlement", name: "settlement", service: newSettlementFixture()})
	online, _ := startMoleTestNode(t, "online", nil, &moleTestPeer{node: "online", name: "online", service: newOnlineFixture()})
	routes := map[cluster.NodeID]string{"online": online, "settlement": settlement}
	main := &moleFaultMain{main: NewMain("mole")}
	address, _ := startMoleTestNode(t, "mole", routes, main)
	routes["mole"] = address
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	_, err := moleRPC(t, routes, "mole", "attack-broken", attackJoinCommand, uint64(2), "login-2", "access-01", "agent", "membership")
	if !errors.Is(err, runtime.ErrServicePanicked) {
		t.Fatalf("fault did not panic group: %v", err)
	}
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
}

// TestMoleSettlementMayRejectCachedBalance 保护缓存预检查通过后，其他业务改变余额仍由结算最终拒绝。
func TestMoleSettlementMayRejectCachedBalance(t *testing.T) {
	routes, _ := prepareMoleCluster(t, newSettlementFixture())
	snapshot := enterMoleTestUser(t, routes, 1, "login", "access-01")
	if _, err := moleRPC(t, routes, "settlement", "settlement", 998, uint64(1), int64(0)); err != nil {
		t.Fatal(err)
	}
	hit := testHitTarget(t, routes, snapshot, 1, "login", "action")
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "rejected")
	value, err := moleRPC(t, routes, "mole", moleAccountingName, accountingLoadCommand, uint64(1))
	if err != nil || value.(map[string]any)["confirmed_balance"] != int64(0) || value.(map[string]any)["available"] != int64(0) {
		t.Fatalf("cache not reconciled: %v %v", value, err)
	}
}

// TestMoleDirectoryIgnoresPreviousMembershipCleanup 保护相同 LoginID 退出再进入时旧清理通知不会删除新成员。
func TestMoleDirectoryIgnoresPreviousMembershipCleanup(t *testing.T) {
	service := newMoleService()
	service.routes[7] = moleRoute{Attack: "attack", LoginID: "login", Membership: "new"}
	service.groups["attack"] = 1
	if service.removeMember(7, "login", "attack", "old") || len(service.routes) != 1 {
		t.Fatal("old cleanup removed new membership")
	}
	if !service.removeMember(7, "login", "attack", "new") || len(service.routes) != 0 || len(service.groups) != 0 {
		t.Fatal("current cleanup failed")
	}
}

// TestMoleReconnectKeepsProjectionWhileReceiptMissing 保护远端已入账但回执缺失时，重连使用本地视图且不再次查询覆盖余额。
func TestMoleReconnectKeepsProjectionWhileReceiptMissing(t *testing.T) {
	fixture := newSettlementFixture()
	fixture.hideReceipts = true
	routes, _ := prepareMoleCluster(t, fixture)
	snapshot := enterMoleTestUser(t, routes, 1, "old-login", "access-01")
	hit := testHitTarget(t, routes, snapshot, 1, "old-login", "action")
	select {
	case <-fixture.submitted:
	case <-time.After(2 * time.Second):
		t.Fatal("batch was not received by settlement")
	}
	reconnected := enterMoleTestUser(t, routes, 1, "new-login", "access-02")
	silver := reconnected["silver"].(map[string]any)
	if silver["balance"] != hit["balance"] || silver["confirmed_balance"] != int64(10000) ||
		silver["available"] != int64(9900) || len(fixture.balanceRequests) != 1 {
		t.Fatalf("reconnect changed pending projection or queried balance again: %v", silver)
	}
	if _, err := moleRPC(t, routes, "settlement", "settlement", 997, false); err != nil {
		t.Fatal(err)
	}
	waitMoleOperation(t, routes, 1, hit["hit_id"].(string), "committed")
	value, err := moleRPC(t, routes, "mole", moleAccountingName, accountingLoadCommand, uint64(1))
	if err != nil {
		t.Fatal(err)
	}
	silver = value.(map[string]any)
	if silver["balance"] != hit["balance"] || silver["confirmed_balance"] != hit["balance"] || silver["available"] != hit["balance"] {
		t.Fatalf("receipt did not reconcile pending projection: %v", silver)
	}
}
