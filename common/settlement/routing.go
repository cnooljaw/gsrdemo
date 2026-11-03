package settlement

import (
	"fmt"
	"hash/fnv"
)

const (
	writeCount = 4
	readCount  = 2
)

// Services 仅供结算节点组装和协议测试取得入口清单；业务使用具体调用方法。
func Services() (writes, reads []string) {
	for index := range writeCount {
		writes = append(writes, fmt.Sprintf("settlement-write-%02d", index))
	}
	for index := range readCount {
		reads = append(reads, fmt.Sprintf("settlement-read-%02d", index))
	}
	return writes, reads
}

// writeService 按稳定业务身份分配入口；它不是钱包锁，正确性仍由 SQL 事务保证。
func writeService(identity string) string { return serviceName(identity, "write", writeCount) }

// readService 将只读等待与写入分开，不经一个同步中转 Service。
func readService(identity string) string { return serviceName(identity, "read", readCount) }

func serviceName(identity, kind string, count uint32) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(identity))
	return fmt.Sprintf("settlement-%s-%02d", kind, hash.Sum32()%count)
}
