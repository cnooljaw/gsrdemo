// Package settlement 只共享真实调用方需要的结算端点路由，不包含节点实现或资金规则。
package settlement

import (
	"fmt"
	"hash/fnv"
)

const (
	writeCount = 4
	readCount  = 2
)

// Services 返回独立快照，节点用它发布入口，调用方不保存临时 ServiceID。
func Services() (writes, reads []string) {
	for index := range writeCount {
		writes = append(writes, fmt.Sprintf("settlement-write-%02d", index))
	}
	for index := range readCount {
		reads = append(reads, fmt.Sprintf("settlement-read-%02d", index))
	}
	return writes, reads
}

// WriteService 按稳定业务身份分配入口；它不是钱包锁，正确性仍由 SQL 事务保证。
func WriteService(identity string) string { return serviceName(identity, "write", writeCount) }

// ReadService 将只读等待与写入分开，不经一个同步中转 Service。
func ReadService(identity string) string { return serviceName(identity, "read", readCount) }

func serviceName(identity, kind string, count uint32) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(identity))
	return fmt.Sprintf("settlement-%s-%02d", kind, hash.Sum32()%count)
}
