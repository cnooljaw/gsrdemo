package main

import (
	"testing"
)

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
