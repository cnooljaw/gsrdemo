package main

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

const (
	agentCallTimeout                           = 3 * time.Second
	loginAuthenticateCommand runtime.CommandID = 1
	onlineBindCommand        runtime.CommandID = 1
	onlineUnbindCommand      runtime.CommandID = 3
	agentReplacedCommand     runtime.CommandID = 101
)

var (
	errLoginRequired       = runtime.NewStatusError(runtime.ErrorClassDomain, "access.login_required", "login required")
	errAlreadyLoggedIn     = runtime.NewStatusError(runtime.ErrorClassDomain, "access.already_logged_in", "use a new connection to login again")
	errEnterRequired       = runtime.NewStatusError(runtime.ErrorClassDomain, "access.enter_required", "enter mole first")
	errInvalidBackendReply = runtime.NewStatusError(runtime.ErrorClassDomain, "access.invalid_backend_reply", "invalid backend reply")
)

// onLogin 只接受凭据，身份和连接地址始终由服务端填写。
func (agent *agent) onLogin(self gsr.CommandContext, reply protocol.Context, request *demopb.LoginRequest) error {
	if agent.userID != 0 {
		return errAlreadyLoggedIn
	}
	value, err := agent.call(self, "login", "login", loginAuthenticateCommand, request.GetUsername(), request.GetPassword())
	if err != nil {
		return err
	}
	userID, ok := value.(uint64)
	if !ok || userID == 0 {
		return errInvalidBackendReply
	}
	loginID, err := connectionID()
	if err != nil {
		return err
	}
	previous, err := agent.call(self, "online", "online", onlineBindCommand, userID, loginID, string(agent.node), string(agent.name))
	if err != nil {
		// Bind 超时可能已经执行；不重发 Bind，只尝试清理该候选身份并结束连接。
		if cleanupErr := self.Cluster().Send("online", "online", onlineUnbindCommand, userID, loginID); cleanupErr != nil {
			_ = self.Logger().Error("未确认登录的清理未接纳", "error", cleanupErr.Error())
		}
		self.Exit()
		return err
	}
	agent.userID, agent.loginID = userID, loginID
	if previous != nil {
		old, ok := previous.(map[string]any)
		if !ok {
			agent.disconnect(self)
			self.Exit()
			return errInvalidBackendReply
		}
		oldNode, nodeOK := old["access_node"].(string)
		oldName, nameOK := old["agent_name"].(string)
		oldLogin, loginOK := old["login_id"].(string)
		if !nodeOK || !nameOK || !loginOK || oldNode == "" || oldName == "" || oldLogin == "" {
			agent.disconnect(self)
			self.Exit()
			return errInvalidBackendReply
		}
		// 不等待旧 Agent；其断线/离开仍按旧 LoginID 有条件清理。
		if err := self.Cluster().Send(cluster.NodeID(oldNode), cluster.ServiceName(oldName), agentReplacedCommand, oldLogin); err != nil {
			_ = self.Logger().Error("旧登录通知未接纳", "error", err.Error())
		}
	}
	return reply.Response(uint32(demopb.MsgId_S2C_LOGIN), &demopb.LoginResponse{UserId: userID})
}

func (agent *agent) call(self gsr.ServiceContext, node cluster.NodeID, name cluster.ServiceName, command runtime.CommandID, args ...any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), agentCallTimeout)
	defer cancel()
	return self.Cluster().Call(ctx, node, name, command, args...)
}

func (agent *agent) replaced(self gsr.CommandContext, loginID string) error {
	if agent.loginID == "" || agent.loginID != loginID {
		return nil
	}
	agent.disconnect(self)
	self.Exit()
	return nil
}

// disconnect 只做有界的尽力清理；进程骤停的残留位置由后续登录覆盖。
func (agent *agent) disconnect(self gsr.ServiceContext) {
	if agent.userID == 0 {
		return
	}
	if agent.attackName != "" {
		if _, err := agent.call(self, "mole", agent.attackName, moleLeaveCommand, agent.userID, agent.loginID); err != nil {
			_ = self.Logger().Error("断线离开地鼠未确认", "error", err.Error())
		}
	}
	if _, err := agent.call(self, "online", "online", onlineUnbindCommand, agent.userID, agent.loginID); err != nil {
		_ = self.Logger().Error("断线解绑未确认", "error", err.Error())
	}
	agent.userID, agent.loginID = 0, ""
	agent.entryName, agent.attackName = "", ""
}
