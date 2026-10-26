package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func wantLocation(loginID, accessNode, agentName string) map[string]any {
	return map[string]any{"login_id": loginID, "access_node": accessNode, "agent_name": agentName}
}

// TestOnlineReplacementKeepsNewAccessBinding 保护跨接入节点替换后，旧连接的迟到解绑不能删除新连接。
func TestOnlineReplacementKeepsNewAccessBinding(t *testing.T) {
	service := newOnlineService()
	previous, err := service.bind(nil, 7, "login-old", "access-01", "agent-1")
	if err != nil || previous != nil {
		t.Fatalf("first bind: previous=%v err=%v", previous, err)
	}
	previous, err = service.bind(nil, 7, "login-new", "access-02", "agent-1")
	if err != nil || !reflect.DeepEqual(previous, wantLocation("login-old", "access-01", "agent-1")) {
		t.Fatalf("replace: previous=%v err=%v", previous, err)
	}
	removed, err := service.unbind(nil, 7, "login-old")
	if err != nil || removed {
		t.Fatalf("late disconnect removed new binding: removed=%v err=%v", removed, err)
	}
	current, err := service.lookup(nil, 7)
	if err != nil || !reflect.DeepEqual(current, wantLocation("login-new", "access-02", "agent-1")) {
		t.Fatalf("new access binding: current=%v err=%v", current, err)
	}
	removed, err = service.unbind(nil, 7, "login-new")
	if err != nil || !removed {
		t.Fatalf("current disconnect: removed=%v err=%v", removed, err)
	}
	current, err = service.lookup(nil, 7)
	if err != nil || current != nil {
		t.Fatalf("lookup after disconnect: current=%v err=%v", current, err)
	}
	removed, err = service.unbind(nil, 7, "login-new")
	if err != nil || removed {
		t.Fatalf("repeated disconnect: removed=%v err=%v", removed, err)
	}
}

// TestOnlineLoginIDCannotMoveBetweenConnections 保护当前绑定的重复提交不替换连接，同一 LoginID 的不同地址被拒绝。
func TestOnlineLoginIDCannotMoveBetweenConnections(t *testing.T) {
	service := newOnlineService()
	if _, err := service.bind(nil, 7, "login-1", "access-01", "agent-1"); err != nil {
		t.Fatal(err)
	}
	previous, err := service.bind(nil, 7, "login-1", "access-01", "agent-1")
	if err != nil || previous != nil {
		t.Fatalf("repeat bind must not report an evicted connection: previous=%v err=%v", previous, err)
	}
	for _, address := range [][2]string{{"access-02", "agent-1"}, {"access-01", "agent-2"}} {
		if _, err := service.bind(nil, 7, "login-1", address[0], address[1]); !errors.Is(err, errLoginIDConflict) {
			t.Fatalf("same login moved to %v: %v", address, err)
		}
	}
	current, err := service.lookup(nil, 7)
	if err != nil || !reflect.DeepEqual(current, wantLocation("login-1", "access-01", "agent-1")) {
		t.Fatalf("conflict changed state: current=%v err=%v", current, err)
	}
}

// TestOnlineReturnsIndependentSnapshots 保护本地调用结果也不暴露目录内部可变状态，且不同用户可使用不同节点内的同名 Agent。
func TestOnlineReturnsIndependentSnapshots(t *testing.T) {
	service := newOnlineService()
	for userID, accessNode := range map[uint64]string{7: "access-01", 8: "access-02"} {
		if _, err := service.bind(nil, userID, "login-1", accessNode, "agent-1"); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := service.lookup(nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.(map[string]any)["access_node"] = "corrupted"
	current, _ := service.lookup(nil, 7)
	if !reflect.DeepEqual(current, wantLocation("login-1", "access-01", "agent-1")) {
		t.Fatalf("lookup exposed state: %v", current)
	}
	previous, err := service.bind(nil, 7, "login-2", "access-03", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	previous.(map[string]any)["agent_name"] = "corrupted"
	current, _ = service.lookup(nil, 7)
	other, _ := service.lookup(nil, 8)
	if !reflect.DeepEqual(current, wantLocation("login-2", "access-03", "agent-1")) ||
		!reflect.DeepEqual(other, wantLocation("login-1", "access-02", "agent-1")) {
		t.Fatalf("replacement or other user corrupted: current=%v other=%v", current, other)
	}
}

// TestOnlineRejectsInvalidInputWithoutChangingBinding 保护标识的长度和字符边界，非法操作不会改写已有绑定。
func TestOnlineRejectsInvalidInputWithoutChangingBinding(t *testing.T) {
	service := newOnlineService()
	if _, err := service.bind(nil, 7, "valid", "access-01", "agent-1"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "has space", "has\nnewline", "\x00", "非ASCII", strings.Repeat("x", 129)} {
		for _, args := range [][3]string{{invalid, "access-01", "agent-1"}, {"new", invalid, "agent-1"}, {"new", "access-01", invalid}} {
			if _, err := service.bind(nil, 7, args[0], args[1], args[2]); !errors.Is(err, errInvalidOnlineInput) {
				t.Fatalf("invalid bind accepted: %q: %v", args, err)
			}
		}
		if _, err := service.unbind(nil, 7, invalid); !errors.Is(err, errInvalidOnlineInput) {
			t.Fatalf("invalid unbind accepted: %q: %v", invalid, err)
		}
	}
	if _, err := service.bind(nil, 0, "login-1", "access-01", "agent-1"); !errors.Is(err, errInvalidOnlineInput) {
		t.Fatalf("zero user bind: %v", err)
	}
	if _, err := service.lookup(nil, 0); !errors.Is(err, errInvalidOnlineInput) {
		t.Fatalf("zero user lookup: %v", err)
	}
	if _, err := service.unbind(nil, 0, "valid"); !errors.Is(err, errInvalidOnlineInput) {
		t.Fatalf("zero user unbind: %v", err)
	}
	current, err := service.lookup(nil, 7)
	if err != nil || !reflect.DeepEqual(current, wantLocation("valid", "access-01", "agent-1")) {
		t.Fatalf("invalid operation changed state: current=%v err=%v", current, err)
	}
	boundary := strings.Repeat("x", 128)
	if _, err := service.bind(nil, 8, boundary, boundary, boundary); err != nil {
		t.Fatalf("valid boundary rejected: %v", err)
	}
}
