package main

import (
	"context"
	"time"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/cluster"
	"github.com/cnooljaw/gsr/gate"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	authrpc "github.com/cnooljaw/gsrdemo/common/rpc/auth"
	onlinerpc "github.com/cnooljaw/gsrdemo/common/rpc/online"
	tokenrpc "github.com/cnooljaw/gsrdemo/common/rpc/token"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

var errAlreadyLoggedIn = runtime.NewStatusError(runtime.ErrorClassDomain, "access.already_logged_in", "use a new connection to login again")

// onPasswordAuth 只完成密码认证并返回凭据，不填写会话身份，也不绑定 Online。
func (agent *agent) onPasswordAuth(self gsr.CommandContext, reply protocol.Context, request *demopb.PasswordAuthRequest) error {
	if agent.session.userID != 0 {
		return errAlreadyLoggedIn
	}
	value, err := callBackend(self, "login", authrpc.ServiceName, authrpc.AuthenticateCommand, request.GetUsername(), request.GetPassword())
	if err != nil {
		return err
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return errInvalidBackendReply
	}
	uid, uidOK := fields["user_id"].(uint64)
	token, tokenOK := fields["access_token"].(string)
	expires, expiresOK := fields["expires_at_ms"].(int64)
	if !uidOK || uid == 0 || !tokenOK || len(token) != 43 || !expiresOK || expires <= time.Now().UnixMilli() {
		return errInvalidBackendReply
	}
	return reply.Response(uint32(demopb.MsgId_S2C_PASSWORD_AUTH), &demopb.PasswordAuthResponse{UserId: uid, AccessToken: token, ExpiresAtMs: expires})
}

// onLogin 只接受平台 Token，身份和连接地址始终由服务端填写。
func (agent *agent) onLogin(self gsr.CommandContext, reply protocol.Context, request *demopb.LoginRequest) error {
	if agent.session.userID != 0 {
		return errAlreadyLoggedIn
	}
	value, err := callBackend(self, "login", tokenrpc.ServiceName, tokenrpc.VerifyCommand, request.GetAccessToken())
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
	previous, err := callBackend(self, "online", onlinerpc.ServiceName, onlinerpc.BindCommand, userID, loginID, string(agent.session.node), string(agent.session.name))
	if err != nil {
		// Bind 超时可能已经执行；不重发 Bind，只尝试清理该候选身份并结束连接。
		if cleanupErr := self.Cluster().Send("online", onlinerpc.ServiceName, onlinerpc.UnbindCommand, userID, loginID); cleanupErr != nil {
			_ = self.Logger().Error("未确认登录的清理未接纳", "error", cleanupErr.Error())
		}
		return err
	}
	agent.session.userID, agent.session.loginID = userID, loginID
	if previous != nil {
		old, ok := previous.(map[string]any)
		if !ok {
			agent.disconnect(self)
			return errInvalidBackendReply
		}
		oldNode, nodeOK := old["access_node"].(string)
		oldName, nameOK := old["agent_name"].(string)
		oldLogin, loginOK := old["login_id"].(string)
		if !nodeOK || !nameOK || !loginOK || oldNode == "" || oldName == "" || oldLogin == "" {
			agent.disconnect(self)
			return errInvalidBackendReply
		}
		// 不等待旧 Agent；其断线/离开仍按旧 LoginID 有条件清理。
		if err := self.Cluster().Send(cluster.NodeID(oldNode), cluster.ServiceName(oldName), replacedCommand, oldLogin); err != nil {
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
	if _, err := callBackend(self, "online", onlinerpc.ServiceName, onlinerpc.UnbindCommand, agent.session.userID, agent.session.loginID); err != nil {
		_ = self.Logger().Error("断线解绑未确认", "error", err.Error())
	}
	agent.session.userID, agent.session.loginID = 0, ""
}

func (agent *agent) commonProtocolHandlers() map[uint32]any {
	return map[uint32]any{
		uint32(demopb.MsgId_C2S_LOGIN):         agent.onLogin,
		uint32(demopb.MsgId_C2S_PASSWORD_AUTH): agent.onPasswordAuth,
	}
}

func (agent *agent) commonCommandHandlers() map[runtime.CommandID]any {
	return map[runtime.CommandID]any{
		replacedCommand: agent.handleReplaced,
	}
}

// 登录失败关闭辅助 Service 仅由本地计时唤醒，不发布远程名称或共享契约。
//
//rpc:service loginfailurecloser private
const loginFailureCloseCommand runtime.CommandID = 1

// loginFailureCloser 独立等待错误回包的短发送窗口；Agent 已封闭后续业务，框架连接队列仍由 Gate 独占。
type loginFailureCloser struct{ client gate.ClientSession }

var _ gsr.Service = (*loginFailureCloser)(nil)

func (closer *loginFailureCloser) OnStart(_ context.Context, self gsr.ServiceContext) error {
	_, err := self.After(200*time.Millisecond, loginFailureCloseCommand)
	return err
}

func (closer *loginFailureCloser) Handle(self gsr.CommandContext, cmd runtime.CommandID, _ ...any) (any, error) {
	if cmd != loginFailureCloseCommand || self.Source() != 0 {
		return nil, runtime.ErrUnknownCommand
	}
	self.Exit()
	return nil, nil
}

func (closer *loginFailureCloser) OnStop(_ context.Context) error {
	return closer.client.Close()
}

func (agent *agent) closeAfterLoginFailure(self gsr.ServiceContext) {
	agent.closing = true
	ctx, cancel := context.WithTimeout(context.Background(), agentCallTimeout)
	defer cancel()
	if _, err := self.CreateService(ctx, &loginFailureCloser{client: agent.client}); err != nil {
		self.Exit()
		return
	}
}
