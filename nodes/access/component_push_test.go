package main

import (
	"errors"
	"testing"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/common/rpc/accessagent"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

// TestGamePushCommands 保护独立推送命令无需字符串子指令，并保留登录、进入状态及快照路由校验。
func TestGamePushCommands(t *testing.T) {
	for _, test := range []struct {
		name     string
		command  runtime.CommandID
		response demopb.MsgId
		routeKey string
		payload  map[string]any
	}{
		{name: "mole snapshot", command: accessagent.MoleSnapshotPushCommand, response: demopb.MsgId_S2C_SNAPSHOT_PUSH, routeKey: "attack_name", payload: map[string]any{"attack_name": "mole-room"}},
		{name: "mole balance correction", command: accessagent.MoleBalanceCorrectionPushCommand, response: demopb.MsgId_S2C_BALANCE_PUSH, payload: map[string]any{"balance": int64(900), "revision": uint64(2)}},
		{name: "landlord snapshot", command: accessagent.LandlordSnapshotPushCommand, response: demopb.MsgId_S2C_LANDLORD_PUSH, routeKey: "room_name", payload: map[string]any{"room_name": "landlord-room"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := agentIdentity{UserID: 7, LoginID: "current-login"}
			pushes := 0
			deps := componentDependencies{Identity: func() agentIdentity { return identity }, Push: func(id uint32, _ proto.Message) error {
				pushes++
				if id != uint32(test.response) {
					t.Fatalf("错误客户端推送编号: %d", id)
				}
				return nil
			}}
			mole, landlord := newComMole(deps), newComLandlord(deps)
			mole.attackName, landlord.room = "mole-room", "landlord-room"
			var component gameComponent = mole
			if test.command == accessagent.LandlordSnapshotPushCommand {
				component = landlord
			}
			handler, ok := component.CommandHandlers()[test.command].(func(gsr.CommandContext, string, map[string]any) error)
			if !ok {
				t.Fatalf("命令 %d 未直接绑定推送处理方法", test.command)
			}
			if err := handler(nil, "old-login", nil); err != nil {
				t.Fatal(err)
			}
			identity.UserID = 0
			if err := handler(nil, identity.LoginID, nil); err != nil {
				t.Fatal(err)
			}
			identity.UserID = 7
			mole.attackName, landlord.room = "", ""
			if err := handler(nil, identity.LoginID, nil); err != nil {
				t.Fatal(err)
			}
			mole.attackName, landlord.room = "mole-room", "landlord-room"
			if test.routeKey != "" {
				if err := handler(nil, identity.LoginID, map[string]any{test.routeKey: "previous-room"}); err != nil {
					t.Fatal(err)
				}
			}
			if pushes != 0 {
				t.Fatalf("陈旧或无效路由推送未被忽略: %d", pushes)
			}
			if err := handler(nil, identity.LoginID, test.payload); err != nil {
				t.Fatal(err)
			}
			if pushes != 1 {
				t.Fatalf("当前推送次数: %d", pushes)
			}
			if err := handler(nil, identity.LoginID, nil); !errors.Is(err, errInvalidBackendReply) {
				t.Fatalf("未拒绝损坏响应: %v", err)
			}
		})
	}
}
