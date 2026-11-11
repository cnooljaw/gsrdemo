package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSource(t *testing.T, root, name, source string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readSource(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestServiceContracts 保护公开粒度属于 Service：不同 Service 可复用编号，私有 Service 不导出，动态实例不生成固定名称。
func TestServiceContracts(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "nodes/example/services.go", `package main
//rpc:service writer public name=writerName
const (
 writerName = "writer"
 writeCommand uint32 = 1
)
//rpc:service room public
const (
 joinCommand uint32 = 1
 tickCommand uint32 = 10
)
//rpc:service pulse private
const ( pulseCommand uint32 = 1 )
const unmarkedCommand = 2
`)
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	writer := readSource(t, filepath.Join(root, "common/rpc/writer/commands.gen.go"))
	room := readSource(t, filepath.Join(root, "common/rpc/room/commands.gen.go"))
	if !strings.Contains(writer, `ServiceName = "writer"`) || !strings.Contains(writer, "WriteCommand = 1") {
		t.Fatalf("固定逻辑名称或命令未生成: %s", writer)
	}
	if strings.Contains(room, "ServiceName") || !strings.Contains(room, "JoinCommand = 1") || !strings.Contains(room, "TickCommand = 10") {
		t.Fatalf("动态 Service 应导出全部命令且不固定实例名称: %s", room)
	}
	if _, err := os.Stat(filepath.Join(root, "common/rpc/pulse")); !os.IsNotExist(err) {
		t.Fatalf("私有 Service 不应生成契约: %v", err)
	}
	if err := run(root, true); err != nil {
		t.Fatalf("刚生成的契约应通过检查: %v", err)
	}
}

// TestInvalidContracts 保护同一 Service 编号唯一、公开契约名唯一、编号显式且不超出协议范围，错误输入不产生文件。
func TestInvalidContracts(t *testing.T) {
	cases := []struct{ name, source, want string }{
		{"duplicate_id", "//rpc:service writer public\nconst (first = 1; second = 1)", "编号重复"},
		{"duplicate_service", "//rpc:service writer public\nconst first = 1\n//rpc:service writer public\nconst second = 2", "契约名 writer 重复"},
		{"iota", "//rpc:service writer public\nconst first = iota", "不支持 iota"},
		{"implicit", "//rpc:service writer public\nconst (first = 1; second)", "显式赋值"},
		{"overflow", "//rpc:service writer public\nconst first = 4294967296", "无效的 CommandID"},
		{"expression", "//rpc:service writer public\nconst first = 1 + 1", "不支持 iota、引用或表达式"},
		{"missing_name", "//rpc:service writer public name=writerName\nconst first = 1", "未在当前命令块定义"},
		{"invalid_package", "//rpc:service ../escape public\nconst first = 1", "标记格式"},
		{"mutable", "//rpc:service writer public\nvar first = 1", "const 块"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			root := t.TempDir()
			writeSource(t, root, "nodes/example/service.go", "package main\n"+item.source+"\n")
			if err := run(root, false); err == nil || !strings.Contains(err.Error(), item.want) {
				t.Fatalf("应拒绝 %s，实际 %v", item.want, err)
			}
			if _, err := os.Stat(filepath.Join(root, "common/rpc")); !os.IsNotExist(err) {
				t.Fatalf("错误输入不应生成任何产物: %v", err)
			}
		})
	}
}

// TestStaleContracts 保护检查模式不写文件，重新生成更新旧编号，Service 转为私有后只清理工具拥有的产物。
func TestStaleContracts(t *testing.T) {
	root := t.TempDir()
	source := "package main\n//rpc:service writer public\nconst writeCommand = 1\n"
	writeSource(t, root, "nodes/example/service.go", source)
	target := filepath.Join(root, "common/rpc/writer/commands.gen.go")
	if err := run(root, true); err == nil {
		t.Fatal("缺失生成文件必须报错")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("检查模式不能创建文件")
	}
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	before := readSource(t, target)
	writeSource(t, root, "nodes/example/service.go", strings.Replace(source, "= 1", "= 2", 1))
	if err := run(root, true); err == nil || readSource(t, target) != before {
		t.Fatal("过期检查应失败且不能更新文件")
	}
	if err := run(root, false); err != nil || !strings.Contains(readSource(t, target), "WriteCommand = 2") {
		t.Fatalf("重新生成应更新编号: %v", err)
	}
	writeSource(t, root, "nodes/example/service.go", strings.Replace(source, "public", "private", 1))
	manual := writeSource(t, root, "common/rpc/writer/manual.go", "package writer\n")
	if err := run(root, true); err == nil {
		t.Fatal("Service 转为私有应发现过期契约")
	}
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("过期的生成文件应清理")
	}
	if readSource(t, manual) != "package writer\n" {
		t.Fatal("不能清理用户手写文件")
	}
}

// TestHandwrittenTarget 保护生成器不会覆盖目标路径中已有的手写文件。
func TestHandwrittenTarget(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root, "nodes/example/service.go", "package main\n//rpc:service writer public\nconst writeCommand = 1\n")
	target := writeSource(t, root, "common/rpc/writer/commands.gen.go", "package writer\n// 手写内容\n")
	if err := run(root, false); err == nil || !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Fatalf("应拒绝覆盖手写目标: %v", err)
	}
	if !strings.Contains(readSource(t, target), "手写内容") {
		t.Fatal("手写内容被修改")
	}
}
