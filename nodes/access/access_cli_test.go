// 真实 TypeScript CLI 行为验证；显式启用进程测试，后端仅由 TCP 替身提供确定性结果。
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAccessCLITargetRefusalAndUnknownResult 保护实际 CLI 不查账也能连续命中，目标失效后换目标，未知结果只发一次后停止。
func TestAccessCLITargetRefusalAndUnknownResult(t *testing.T) {
	if os.Getenv("GSRDEMO_ACCESS_PROCESSES") != "1" {
		t.Skip("build typeclient and set GSRDEMO_ACCESS_PROCESSES=1")
	}
	for _, scenario := range []string{"target-first", "timeout-first", "busy-first"} {
		t.Run(scenario, func(t *testing.T) {
			gateAddress, _, hits := startAccessBusiness(t, scenario)
			clientPath, err := filepath.Abs("../../typeclient/dist/index.js")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "node", clientPath, "--url", "ws://"+gateAddress+"/ws", "--username", "alice", "--hits", "2")
			command.Stdin = strings.NewReader("password\n")
			output, runErr := command.CombinedOutput()
			if scenario == "timeout-first" || scenario == "busy-first" {
				if runErr == nil || strings.Contains(string(output), `"event":"miss"`) || len(hits) != 1 {
					t.Fatalf("未知结果不能继续: %v %s hits=%d", runErr, output, len(hits))
				}
				return
			}
			if runErr != nil || !strings.Contains(string(output), `"event":"leave"`) || strings.Contains(string(output), `"event":"settlement"`) || len(hits) != 3 {
				t.Fatalf("CLI 未独立于账务查询完成命中: %v %s hits=%d", runErr, output, len(hits))
			}

			if !strings.Contains(string(output), `"event":"miss"`) {
				t.Fatal("未记录目标失效", string(output))
			}
			first, second := <-hits, <-hits
			if first[2] == second[2] || first[3] != uint64(9) || second[3] != uint64(10) {
				t.Fatalf("应刷新目标、使用新动作: %v %v", first, second)
			}
		})
	}
}

// TestAccessCLITokenFileAuthentication 保护真实 CLI 显式保存私密 Token 文件后可跳过密码认证，损坏文件拒绝读取且不泄露内容。
func TestAccessCLITokenFileAuthentication(t *testing.T) {
	if os.Getenv("GSRDEMO_ACCESS_PROCESSES") != "1" {
		t.Skip("build typeclient and set GSRDEMO_ACCESS_PROCESSES=1")
	}
	address, _, hits := startAccessBusiness(t)
	clientPath, err := filepath.Abs("../../typeclient/dist/index.js")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	command := exec.CommandContext(ctx, "node", clientPath, "--url", "ws://"+address+"/ws", "--username", "alice", "--hits", "2", "--save-token-file", tokenFile)
	command.Stdin = strings.NewReader("password\n")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), `"event":"leave"`) || len(hits) != 2 {
		t.Fatalf("密码认证及保存 Token 失败: %v %s", err, output)
	}
	info, err := os.Stat(tokenFile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("Token 文件权限不正确", err)
	}
	if strings.Contains(string(output), strings.Repeat("A", 43)) {
		t.Fatal("Token 被写入日志")
	}
	reuse := exec.CommandContext(ctx, "node", clientPath, "--url", "ws://"+address+"/ws", "--token-file", tokenFile, "--hits", "2")
	reused, err := reuse.CombinedOutput()
	if err != nil || strings.Contains(string(reused), `"event":"authenticated"`) || !strings.Contains(string(reused), `"event":"leave"`) || len(hits) != 4 {
		t.Fatalf("复用 Token 未跳过密码认证: %v %s hits=%d", err, reused, len(hits))
	}
	if err := os.WriteFile(tokenFile, []byte(strings.Repeat("A", 43)), 0600); err != nil {
		t.Fatal(err)
	}
	broken := exec.CommandContext(ctx, "node", clientPath, "--url", "ws://"+address+"/ws", "--token-file", tokenFile)
	failure, err := broken.CombinedOutput()
	if err == nil || strings.Contains(string(failure), strings.Repeat("A", 8)) || !strings.Contains(string(failure), "cannot read a valid token file") {
		t.Fatal("损坏凭据文件未安全拒绝或泄露内容")
	}
}
