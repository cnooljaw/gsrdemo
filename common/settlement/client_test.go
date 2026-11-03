package settlement

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

type recordedCall struct {
	node    cluster.NodeID
	name    cluster.ServiceName
	command runtime.CommandID
	args    []any
	budget  time.Duration
	calls   int
	result  any
	err     error
}

var _ ClusterCaller = (*recordedCall)(nil)

func (call *recordedCall) Call(ctx context.Context, node cluster.NodeID, name cluster.ServiceName, command runtime.CommandID, args ...any) (any, error) {
	call.node, call.name, call.command, call.args = node, name, command, args
	deadline, _ := ctx.Deadline()
	call.budget = time.Until(deadline)
	call.calls++
	return call.result, call.err
}

// TestClientBusinessMethods 保护各业务方法选择正确的读写能力，并原样传递规范参数和结果。
func TestClientBusinessMethods(t *testing.T) {
	batch := map[string]any{"batch_id": "batch-a", "operations": []any{}}
	operation := map[string]any{"operation_id": "op-a", "user_id": uint64(7)}
	cases := []struct {
		name    string
		invoke  func(context.Context, ClusterCaller) (any, error)
		command runtime.CommandID
		args    []any
		write   bool
	}{
		{"batch", func(ctx context.Context, c ClusterCaller) (any, error) { return SettleBatch(ctx, c, batch) }, 1, []any{batch}, true},
		{"single", func(ctx context.Context, c ClusterCaller) (any, error) { return SettleSingle(ctx, c, operation) }, 6, []any{operation}, true},
		{"group", func(ctx context.Context, c ClusterCaller) (any, error) { return SettleGroup(ctx, c, operation) }, 7, []any{operation}, true},
		{"get batch", func(ctx context.Context, c ClusterCaller) (any, error) { return GetBatch(ctx, c, "batch-a") }, 2, []any{"batch-a"}, false},
		{"get operation", func(ctx context.Context, c ClusterCaller) (any, error) { return GetOperation(ctx, c, 7, "op-a") }, 3, []any{uint64(7), "op-a"}, false},
		{"get balance", func(ctx context.Context, c ClusterCaller) (any, error) { return GetBalance(ctx, c, 7) }, 4, []any{uint64(7)}, false},
	}
	writes, reads := Services()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := &recordedCall{result: "receipt"}
			value, err := tc.invoke(context.Background(), call)
			if err != nil || value != "receipt" || call.calls != 1 || call.node != "settlement" || call.command != tc.command || !reflect.DeepEqual(call.args, tc.args) {
				t.Fatalf("调用契约变化: %+v value=%v err=%v", call, value, err)
			}
			names := reads
			if tc.write {
				names = writes
			}
			found := false
			for _, name := range names {
				found = found || string(call.name) == name
			}
			if !found || call.budget <= 0 || call.budget > 3*time.Second {
				t.Fatalf("入口或有限等待错误: %+v", call)
			}
		})
	}
}

// TestClientPreservesFailureAndBudget 保护封装不自动重试或吞错，并尊重业务更短的等待预算。
func TestClientPreservesFailureAndBudget(t *testing.T) {
	unknown := runtime.ErrCallTimeout
	call := &recordedCall{err: unknown}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := GetBalance(ctx, call, 7); !errors.Is(err, unknown) || call.calls != 1 || call.budget > 100*time.Millisecond {
		t.Fatalf("封装改变未知结果或等待预算: %+v %v", call, err)
	}
	call = &recordedCall{}
	if _, err := SettleBatch(ctx, call, map[string]any{"batch_id": uint64(7)}); err == nil || call.calls != 0 {
		t.Fatal("畸形身份未在路由前拒绝")
	}
	if value, err := GetOperation(ctx, call, 7, "missing"); err != nil || value != nil {
		t.Fatalf("查询缺失语义变化: %v %v", value, err)
	}
}
