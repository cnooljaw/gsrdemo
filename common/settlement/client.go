// Package settlement 提供通用银子结算调用；业务不选择读写入口，不依赖节点实现。
package settlement

import (
	"context"
	"strconv"
	"time"

	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/runtime"
)

// ClusterCaller 是 GSR Cluster 已有的 Call 能力；仅在当前 Service 回调中借用，不保存。
type ClusterCaller interface {
	Call(context.Context, cluster.NodeID, cluster.ServiceName, runtime.CommandID, ...any) (any, error)
}

const callTimeout = 3 * time.Second

// SettleBatch 提交多笔独立单人操作，成功返回已提交的终局回执。
func SettleBatch(ctx context.Context, caller ClusterCaller, request map[string]any) (any, error) {
	return write(ctx, caller, 1, "batch_id", request)
}

// SettleSingle 提交一笔单人操作，调用方保留原 OperationID 处理未知结果。
func SettleSingle(ctx context.Context, caller ClusterCaller, request map[string]any) (any, error) {
	return write(ctx, caller, 6, "operation_id", request)
}

// SettleGroup 提交一笔多人整体操作，所有参与用户一起成功或拒绝。
func SettleGroup(ctx context.Context, caller ClusterCaller, request map[string]any) (any, error) {
	return write(ctx, caller, 7, "operation_id", request)
}

// GetBatch 查询原批次回执，缺失返回 nil。
func GetBatch(ctx context.Context, caller ClusterCaller, batchID string) (any, error) {
	return call(ctx, caller, readService(batchID), 2, batchID)
}

// GetOperation 查询参与用户的原操作，缺失或非参与用户返回 nil。
func GetOperation(ctx context.Context, caller ClusterCaller, userID uint64, operationID string) (any, error) {
	return call(ctx, caller, readService(operationID), 3, userID, operationID)
}

// GetBalance 查询权威余额；钱包不存在返回错误，不自动创建。
func GetBalance(ctx context.Context, caller ClusterCaller, userID uint64) (any, error) {
	return call(ctx, caller, readService(strconv.FormatUint(userID, 10)), 4, userID)
}

func write(ctx context.Context, caller ClusterCaller, command runtime.CommandID, field string, request map[string]any) (any, error) {
	identity, ok := request[field].(string)
	if !ok || identity == "" {
		return nil, runtime.NewStatusError(runtime.ErrorClassDomain, "settlement.invalid_request", "settlement identity is required")
	}
	return call(ctx, caller, writeService(identity), command, request)
}

// call 只封装一次有限 Call，不重试、不把超时解释成财务失败。
// 业务可通过 ctx 缩短等待；参数和回执字段遵循 PROTOCOL.md，不另建编解码层。
func call(ctx context.Context, caller ClusterCaller, name string, command runtime.CommandID, args ...any) (any, error) {
	wait, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return caller.Call(wait, "settlement", cluster.ServiceName(name), command, args...)
}
