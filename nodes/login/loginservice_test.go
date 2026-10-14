package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/logging"
	"github.com/cnooljaw/gsr/runtime"
)

// observedMain 只观察真实 MainService 的启动结果，保留实际创建和命名流程。
type observedMain struct {
	service gsr.Service
	ready   chan<- string
}

var _ gsr.Service = (*observedMain)(nil)

func (service *observedMain) OnStart(ctx context.Context, self gsr.ServiceContext) error {
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

func (service *observedMain) Handle(self gsr.CommandContext, command runtime.CommandID, args ...any) (any, error) {
	return service.service.Handle(self, command, args...)
}

func (service *observedMain) OnStop(ctx context.Context) error {
	return service.service.OnStop(ctx)
}

func startLogin(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	main := &observedMain{service: NewMain("login", db), ready: ready}
	directory := t.TempDir()
	go func() {
		done <- gsr.Run(ctx, gsr.Config{
			Logging: logging.Config{Directory: directory},
			Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"login": "127.0.0.1:0"}},
		}, main)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("login shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("login shutdown timed out")
		}
	})
	select {
	case address := <-ready:
		return address
	case err := <-done:
		// 留回结果供 cleanup 统一回收。
		done <- err
		t.Fatalf("login startup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("login startup timed out")
	}
	return ""
}

// loginCaller 用普通 Service 身份发起真实 Cluster 请求，不使用 Runtime 注入捷径。
type loginCaller struct {
	call   func(gsr.ServiceContext)
	cancel context.CancelFunc
}

var _ gsr.Service = (*loginCaller)(nil)

func (service *loginCaller) OnStart(_ context.Context, self gsr.ServiceContext) error {
	service.call(self)
	service.cancel()
	self.Exit()
	return nil
}

func (service *loginCaller) Handle(_ gsr.CommandContext, _ runtime.CommandID, _ ...any) (any, error) {
	return nil, runtime.ErrUnknownCommand
}

func (service *loginCaller) OnStop(_ context.Context) error {
	return nil
}

func callLogin(t *testing.T, address string, args ...any) (any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var value any
	var callErr error
	caller := &loginCaller{cancel: cancel, call: func(self gsr.ServiceContext) {
		wait, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		value, callErr = self.Cluster().Call(wait, "login", loginServiceName, authenticateCommand, args...)
	}}
	if err := gsr.Run(ctx, gsr.Config{
		Logging: logging.Config{Directory: t.TempDir()},
		Cluster: gsr.ClusterConfig{Nodes: map[cluster.NodeID]string{"login": address}},
	}, caller); err != nil {
		t.Fatal(err)
	}
	return value, callErr
}

// TestLoginAuthenticatesOverCluster 保护真实远程接线、稳定 UserID、错误凭据、输入拒绝和安全数据库错误。
func TestLoginAuthenticatesOverCluster(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	address := startLogin(t, db)
	hash := sha256.Sum256([]byte("example-password"))
	for _, password := range []string{"example-password", "example-password", "wrong"} {
		mock.ExpectQuery(selectAccountSQL).WithArgs("alice").WillReturnRows(
			sqlmock.NewRows([]string{"user_id", "password_hash"}).AddRow(7, hash[:]))
		value, err := callLogin(t, address, "alice", password)
		if password == "wrong" {
			if !errors.Is(err, errInvalidCredentials) {
				t.Fatalf("wrong password: %v", err)
			}
		} else if err != nil || value != uint64(7) {
			t.Fatalf("authenticate: value=%v err=%v", value, err)
		}
	}
	mock.ExpectQuery(selectAccountSQL).WithArgs("missing").WillReturnRows(sqlmock.NewRows([]string{"user_id", "password_hash"}))
	if _, err := callLogin(t, address, "missing", "example-password"); !errors.Is(err, errInvalidCredentials) {
		t.Fatalf("missing account: %v", err)
	}
	for _, args := range [][]any{{"", "password"}, {"alice", ""}, {" alice", "password"}, {"alice", strings.Repeat("p", 129)}} {
		if _, err := callLogin(t, address, args...); !errors.Is(err, errInvalidLoginInput) {
			t.Fatalf("invalid input: %v", err)
		}
	}
	if _, err := callLogin(t, address, uint64(7), "password"); !errors.Is(err, runtime.ErrInvalidCommandArguments) {
		t.Fatalf("wrong wire argument: %v", err)
	}
	mock.ExpectQuery(selectAccountSQL).WithArgs("alice").WillReturnError(fmt.Errorf("private SQL error"))
	if _, err := callLogin(t, address, "alice", "password"); !errors.Is(err, errAccountStoreUnavailable) || strings.Contains(err.Error(), "private SQL") {
		t.Fatalf("database error: %v", err)
	}
	mock.ExpectQuery(selectAccountSQL).WithArgs("broken").WillReturnRows(
		sqlmock.NewRows([]string{"user_id", "password_hash"}).AddRow(7, []byte{1}))
	if _, err := callLogin(t, address, "broken", "password"); !errors.Is(err, errAccountStoreUnavailable) {
		t.Fatalf("corrupt account: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestCreateAccountStoresHash 保护管理入口只存密码摘要，重复账号不会覆盖原账号。
func TestCreateAccountStoresHash(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hash := sha256.Sum256([]byte("example-password"))
	mock.ExpectExec(insertAccountSQL).WithArgs("alice", hash[:]).WillReturnResult(sqlmock.NewResult(7, 1))
	id, err := createAccount(context.Background(), db, "alice", "example-password")
	if err != nil || id != 7 {
		t.Fatalf("create: id=%d err=%v", id, err)
	}
	mock.ExpectExec(insertAccountSQL).WithArgs("alice", hash[:]).WillReturnError(errors.New("duplicate account"))
	if _, err := createAccount(context.Background(), db, "alice", "example-password"); err == nil {
		t.Fatal("duplicate insert reported success")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
