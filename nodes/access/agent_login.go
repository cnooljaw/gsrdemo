package main

import (
	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	"github.com/cnooljaw/gsrdemo/nodes/access/gameagent"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

const (
	loginAuthenticateCommand runtime.CommandID = 1
	onlineBindCommand        runtime.CommandID = 1
	onlineUnbindCommand      runtime.CommandID = 3
	agentReplacedCommand     runtime.CommandID = 101
)

var errAlreadyLoggedIn = runtime.NewStatusError(runtime.ErrorClassDomain, "access.already_logged_in", "use a new connection to login again")

// onLogin 只接受凭据，身份和连接地址始终由服务端填写。
func (agent *agent) onLogin(self gsr.CommandContext, reply protocol.Context, request *demopb.LoginRequest) error {
	if agent.session.userID != 0 {
		return errAlreadyLoggedIn
	}
	value, err := callBackend(self, "login", "login", loginAuthenticateCommand, request.GetUsername(), request.GetPassword())
	if err != nil {
		return err
	}
	userID, ok := value.(uint64)
	if !ok || userID == 0 {
		return gameagent.ErrInvalidBackendReply
	}
	loginID, err := connectionID()
	if err != nil {
		return err
	}
	previous, err := callBackend(self, "online", "online", onlineBindCommand, userID, loginID, string(agent.session.node), string(agent.session.name))
	if err != nil {
		// Bind 超时可能已经执行；不重发 Bind，只尝试清理该候选身份并结束连接。
		if cleanupErr := self.Cluster().Send("online", "online", onlineUnbindCommand, userID, loginID); cleanupErr != nil {
			_ = self.Logger().Error("未确认登录的清理未接纳", "error", cleanupErr.Error())
		}
		self.Exit()
		return err
	}
	agent.session.userID, agent.session.loginID = userID, loginID
	if previous != nil {
		old, ok := previous.(map[string]any)
		if !ok {
			agent.disconnect(self)
			self.Exit()
			return gameagent.ErrInvalidBackendReply
		}
		oldNode, nodeOK := old["access_node"].(string)
		oldName, nameOK := old["agent_name"].(string)
		oldLogin, loginOK := old["login_id"].(string)
		if !nodeOK || !nameOK || !loginOK || oldNode == "" || oldName == "" || oldLogin == "" {
			agent.disconnect(self)
			self.Exit()
			return gameagent.ErrInvalidBackendReply
		}
		// 不等待旧 Agent；其断线/离开仍按旧 LoginID 有条件清理。
		if err := self.Cluster().Send(cluster.NodeID(oldNode), cluster.ServiceName(oldName), agentReplacedCommand, oldLogin); err != nil {
			_ = self.Logger().Error("旧登录通知未接纳", "error", err.Error())
		}
	}
	return reply.Response(uint32(demopb.MsgId_S2C_LOGIN), &demopb.LoginResponse{UserId: userID})
}

func (agent *agent) handleReplaced(self gsr.CommandContext, loginID string) error {
	if agent.session.loginID == "" || agent.session.loginID != loginID {
		return nil
	}
	agent.disconnect(self)
	self.Exit()
	return nil
}

// disconnect 只做有界的尽力清理；进程骤停的残留位置由后续登录覆盖。
func (agent *agent) disconnect(self gsr.ServiceContext) {
	if agent.session.userID == 0 {
		return
	}
	for _, game := range agent.games {
		if err := game.Disconnect(self); err != nil {
			_ = self.Logger().Error("断线离开游戏未确认", "game", game.Name(), "error", err.Error())
		}
	}
	if _, err := callBackend(self, "online", "online", onlineUnbindCommand, agent.session.userID, agent.session.loginID); err != nil {
		_ = self.Logger().Error("断线解绑未确认", "error", err.Error())
	}
	agent.session.userID, agent.session.loginID = 0, ""
}

func (agent *agent) commonProtocolHandlers() map[uint32]any {
	return map[uint32]any{
		uint32(demopb.MsgId_C2S_LOGIN): agent.onLogin,
	}
}

func (agent *agent) commonCommandHandlers() map[runtime.CommandID]any {
	return map[runtime.CommandID]any{
		agentReplacedCommand: agent.handleReplaced,
	}
}
