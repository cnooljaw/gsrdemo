package main

import (
	"errors"
	"testing"

	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

// TestHandlerGroupsRejectDuplicateIDs 保护通用与游戏注册表发生 ID 冲突时初始化失败，不按组装顺序覆盖。
func TestHandlerGroupsRejectDuplicateIDs(t *testing.T) {
	for name, merge := range map[string]func(){
		"protocol": func() { mustMergeHandlers(map[uint32]any{1001: "common"}, map[uint32]any{1001: "game"}) },
		"command": func() {
			mustMergeHandlers(map[runtime.CommandID]any{100: "first"}, map[runtime.CommandID]any{100: "second"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("重复 ID 被静默覆盖")
				}
			}()
			merge()
		})
	}
}

// TestAgentGameAdmissionAndSharedIdentity 保护两游戏互斥、通用登录不依赖房间，以及组件读取当前唯一身份而非构造时的副本。
func TestAgentGameAdmissionAndSharedIdentity(t *testing.T) {
	registry, err := protocol.NewRegistry(demopb.MessageCatalogDemoV1Game)
	if err != nil {
		t.Fatal(err)
	}
	agent := newAgent(&testClientSession{}, registry)
	snapshot := func() error {
		return agent.landlord.onSnapshot(nil, protocol.Context{}, &demopb.LandlordSnapshotRequest{})
	}
	if err := snapshot(); !errors.Is(err, errLoginRequired) {
		t.Fatal("未登录可访问斗地主", err)
	}
	agent.session.userID, agent.session.loginID = 7, "current-login"
	if err := snapshot(); !errors.Is(err, errEnterRequired) {
		t.Fatal("组件没有读到登录后的身份", err)
	}
	agent.mole.attackName = "mole-room"
	if err := agent.allowEnter("mole"); err != nil {
		t.Fatal("当前游戏不能重新进入", err)
	}
	if err := agent.landlord.onEnter(nil, protocol.Context{}, &demopb.LandlordEnterRequest{}); !errors.Is(err, errEnterRequired) {
		t.Fatal("地鼠中仍可进入斗地主", err)
	}
	if err := agent.onLogin(nil, protocol.Context{}, &demopb.LoginRequest{}); !errors.Is(err, errAlreadyLoggedIn) {
		t.Fatal("通用登录错误依赖游戏准入", err)
	}
	agent.mole.attackName, agent.landlord.room = "", "landlord-room"
	if err := agent.allowEnter("landlord"); err != nil {
		t.Fatal("当前斗地主桌不能重新进入", err)
	}
	if err := agent.mole.onEnter(nil, protocol.Context{}, &demopb.EnterRequest{}); !errors.Is(err, errEnterRequired) {
		t.Fatal("斗地主中仍可进入地鼠", err)
	}
	agent.session.userID, agent.session.loginID = 0, ""
	if err := snapshot(); !errors.Is(err, errLoginRequired) {
		t.Fatal("清理身份后组件仍持有旧登录", err)
	}
}
