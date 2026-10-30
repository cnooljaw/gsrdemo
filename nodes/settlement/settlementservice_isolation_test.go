package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
	routing "github.com/cnooljaw/gsrdemo/common/settlement"
)

// TestSettlementSlowSQLIsLocal 保护慢写入或慢查询仅占住当前入口，无关写入和查询不等待它。
func TestSettlementSlowSQLIsLocal(t *testing.T) {
	for _, kind := range []string{"write", "read"} {
		t.Run(kind, func(t *testing.T) {
			started := make(chan struct{})
			var once sync.Once
			pattern := "SELECT request_hash, receipt_json"
			if kind == "read" {
				pattern = "SELECT receipt_json FROM"
			}
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(expected, actual string) error {
				err := sqlmock.QueryMatcherRegexp.Match(expected, actual)
				if err == nil && strings.Contains(actual, pattern) {
					once.Do(func() { close(started) })
				}
				return err
			})))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Error(err)
				}
				_ = db.Close()
			})
			mock.MatchExpectationsInOrder(false)
			writes, reads := routing.Services()
			name, command, args := writes[0], settleBatchCommand, []any{testBatch("slow", testSingle("slow-op", 1, 100, 0))}
			if kind == "write" {
				mock.ExpectBegin()
				mock.ExpectRollback()
			} else {
				name, command, args = reads[0], getBatchCommand, []any{"slow"}
			}
			mock.ExpectQuery(pattern).WithArgs("slow").WillDelayFor(650 * time.Millisecond).WillReturnError(errors.New("injected slow SQL"))
			mock.ExpectQuery("SELECT amount, version").WithArgs(uint64(2)).WillReturnRows(sqlmock.NewRows([]string{"amount", "version"}).AddRow(1000, 1))
			mock.ExpectBegin().WillReturnError(errors.New("independent write reached SQL"))
			address, _ := startSettlementTestNode(t, db)
			routes := map[cluster.NodeID]string{"settlement": address}
			done := make(chan error, 1)
			go func() { _, err := businessRPC(t, routes, "settlement", name, command, args...); done <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("slow SQL did not start")
			}
			// 只读和写入各用另一入口，且应在慢 SQL 返回之前实际到达数据库。
			begin := time.Now()
			value, err := businessRPC(t, routes, "settlement", reads[1], getBalanceCommand, uint64(2))
			if err != nil || value.(map[string]any)["amount"] != int64(1000) {
				t.Fatalf("read blocked: %v %v", value, err)
			}
			if _, err := businessRPC(t, routes, "settlement", writes[1], settleBatchCommand, testBatch("fast", testSingle("fast-op", 2, 100, 0))); !errors.Is(err, errSettlementUnavailable) {
				t.Fatalf("write did not reach independent SQL: %v", err)
			}
			if elapsed := time.Since(begin); elapsed >= 300*time.Millisecond {
				t.Errorf("unrelated requests waited for slow SQL: %v", elapsed)
			}
			if err := <-done; !errors.Is(err, errSettlementUnavailable) {
				t.Fatalf("slow SQL result: %v", err)
			}
		})
	}
}

type settlementPanicFixture struct {
	inner gsr.Service
	name  string
}

var _ gsr.Service = (*settlementPanicFixture)(nil)

func (service *settlementPanicFixture) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	return service.inner.OnStart(ctx, self)
}
func (service *settlementPanicFixture) OnStop(ctx context.Context) error {
	return service.inner.OnStop(ctx)
}
func (service *settlementPanicFixture) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	if command == 998 {
		panic("injected executor panic")
	}
	if command == 997 {
		return service.name, nil
	}
	return service.inner.Handle(self, command, args...)
}

type settlementPanicMain struct {
	db    *sql.DB
	ready chan string
}

var _ gsr.Service = (*settlementPanicMain)(nil)

func (service *settlementPanicMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
	writes, reads := routing.Services()
	var targets []settlementTarget
	for _, name := range append(writes, reads...) {
		target := settlementTarget{name: cluster.ServiceName(name), readOnly: strings.Contains(name, "-read-")}
		id, err := self.CreateService(ctx, &settlementPanicFixture{inner: target.service(service.db), name: name})
		if err != nil {
			return err
		}
		target.id = id
		targets = append(targets, target)
	}
	if _, err := self.CreateService(ctx, &settlementSupervisor{db: service.db, targets: targets}); err != nil {
		return err
	}
	if err := self.Node().OpenCluster("settlement"); err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(self.Node().ClusterListenAddress())
	if err != nil {
		return err
	}
	service.ready <- net.JoinHostPort("127.0.0.1", port)
	self.Exit()
	return nil
}
func (service *settlementPanicMain) Handle(gsr.CommandContext, runtime.CommandID, ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}
func (service *settlementPanicMain) OnStop(context.Context) error { return nil }

// TestSettlementWorkerPanicRestartsOnlyFailedEntry 保护执行入口 panic 后局部恢复同名实例，其他入口与节点继续运行。
func TestSettlementWorkerPanicRestartsOnlyFailedEntry(t *testing.T) {
	for _, kind := range []string{"write", "read"} {
		t.Run(kind, func(t *testing.T) {
			store, mock := mockStore(t)
			main := &settlementPanicMain{db: store.db, ready: make(chan string, 1)}
			address, _ := startSettlementMain(t, main, main.ready)
			routes := map[cluster.NodeID]string{"settlement": address}
			writes, reads := routing.Services()
			target, other := writes[0], reads[0]
			if kind == "read" {
				target, other = other, target
			}
			if _, err := businessRPC(t, routes, "settlement", target, 998); !errors.Is(err, runtime.ErrServicePanicked) {
				t.Fatalf("injection did not panic: %v", err)
			}
			value, err := businessRPC(t, routes, "settlement", other, 997)
			if err != nil || value != other {
				t.Fatalf("other entry stopped: %v %v", value, err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				_, err := businessRPC(t, routes, "settlement", target, 997)
				if errors.Is(err, runtime.ErrUnknownCommand) {
					break
				} // 新实例没有测试包装命令。
				if time.Now().After(deadline) {
					t.Fatalf("entry did not recover: %v", err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if kind == "write" {
				mock.ExpectBegin().WillReturnError(errors.New("recovered entry reached SQL"))
				if _, err := businessRPC(t, routes, "settlement", target, settleBatchCommand, testBatch("after-panic", testSingle("op", 1, 100, 0))); !errors.Is(err, errSettlementUnavailable) {
					t.Fatalf("replacement cannot execute: %v", err)
				}
			} else {
				mock.ExpectQuery("SELECT amount, version").WithArgs(uint64(1)).WillReturnRows(sqlmock.NewRows([]string{"amount", "version"}).AddRow(1000, 1))
				value, err := businessRPC(t, routes, "settlement", target, getBalanceCommand, uint64(1))
				if err != nil || value.(map[string]any)["amount"] != int64(1000) {
					t.Fatalf("replacement cannot query: %v %v", value, err)
				}
			}
			value, err = businessRPC(t, routes, "settlement", other, 997)
			if err != nil || value != other {
				t.Fatal("healthy entry was replaced unnecessarily")
			}
		})
	}
}

// TestSettlementReadWriteBoundaries 保护查询入口不能写账，写入口不承担读取等待。
func TestSettlementReadWriteBoundaries(t *testing.T) {
	store, _ := mockStore(t)
	address, _ := startSettlementTestNode(t, store.db)
	routes := map[cluster.NodeID]string{"settlement": address}
	writes, reads := routing.Services()
	if _, err := businessRPC(t, routes, "settlement", reads[0], settleBatchCommand, testBatch("bad", testSingle("op", 1, 100, 0))); !errors.Is(err, runtime.ErrUnknownCommand) {
		t.Fatal(err)
	}
	if _, err := businessRPC(t, routes, "settlement", writes[0], getBalanceCommand, uint64(1)); !errors.Is(err, runtime.ErrUnknownCommand) {
		t.Fatal(err)
	}
}
