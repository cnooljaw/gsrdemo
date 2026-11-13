package main

import (
	"testing"

	commonv1 "github.com/cnooljaw/gsr/protocol/gen/go/gsr/common/v1"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/proto"
)

func accessAuthenticate(t *testing.T, client *accessClient, username, password string) *demopb.PasswordAuthResponse {
	t.Helper()
	var result demopb.PasswordAuthResponse
	accessProcessResponse(t, client, demopb.MsgId_C2S_PASSWORD_AUTH, &demopb.PasswordAuthRequest{Username: username, Password: password}, &result)
	return &result
}
func accessLogin(t *testing.T, client *accessClient, username, password string) *commonv1.Envelope {
	t.Helper()
	envelope := accessRequest(t, client, demopb.MsgId_C2S_PASSWORD_AUTH, &demopb.PasswordAuthRequest{Username: username, Password: password})
	if envelope.MsgId != uint32(demopb.MsgId_S2C_PASSWORD_AUTH) {
		return envelope
	}
	var auth demopb.PasswordAuthResponse
	if err := proto.Unmarshal(envelope.Payload, &auth); err != nil {
		t.Fatal(err)
	}
	return accessRequest(t, client, demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{AccessToken: auth.AccessToken})
}
func accessLoginResponse(t *testing.T, client *accessClient, username, password string, result *demopb.LoginResponse) {
	t.Helper()
	envelope := accessLogin(t, client, username, password)
	if envelope.MsgId != uint32(demopb.MsgId_S2C_LOGIN) {
		t.Fatal("Token 登录失败", envelope.MsgId)
	}
	if err := proto.Unmarshal(envelope.Payload, result); err != nil {
		t.Fatal(err)
	}
}

// TestAccessAuthenticationIsNotLogin 保护密码认证不绑定 Online、不开放游戏，Token 登录才建立会话。
func TestAccessAuthenticationIsNotLogin(t *testing.T) {
	address, backend, _ := startAccessBusiness(t)
	client := dialAccess(t, address)
	auth := accessAuthenticate(t, client, "alice", "password")
	if auth.AccessToken == "" || auth.UserId != 7 {
		t.Fatal("缺少认证凭据")
	}
	if current := accessFixtureRPC(t, backend, 99); current != nil {
		t.Fatal("认证提前绑定 Online")
	}
	if response := accessRequest(t, client, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}); response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
		t.Fatal("认证后未登录即可进入游戏")
	}
	accessProcessResponse(t, client, demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{AccessToken: auth.AccessToken}, &demopb.LoginResponse{})
	if current := accessFixtureRPC(t, backend, 99); current == nil {
		t.Fatal("Token 登录未绑定")
	}
}

// TestAccessAuthFailureClosesOnlyFailedConnection 保护错误密码/无效 Token 返回错误并关闭当前连接，旧在线用户仍可游戏。
func TestAccessAuthFailureClosesOnlyFailedConnection(t *testing.T) {
	address, backend, _ := startAccessBusiness(t)
	healthy := dialAccess(t, address)
	accessLoginResponse(t, healthy, "alice", "password", &demopb.LoginResponse{})
	before := accessFixtureRPC(t, backend, 99).(map[string]any)
	for _, tokenFailure := range []bool{false, true} {
		failed := dialAccess(t, address)
		var response *commonv1.Envelope
		if tokenFailure {
			response = accessRequest(t, failed, demopb.MsgId_C2S_LOGIN, &demopb.LoginRequest{AccessToken: "bad"})
		} else {
			response = accessRequest(t, failed, demopb.MsgId_C2S_PASSWORD_AUTH, &demopb.PasswordAuthRequest{Username: "alice", Password: "wrong"})
		}
		if response.MsgId != uint32(commonv1.MsgId_S2C_ERROR_RESPONSE) {
			t.Fatal("未收到认证错误")
		}
		accessExpectNoMoreMessages(t, failed)
		if current := accessFixtureRPC(t, backend, 99).(map[string]any); current["login_id"] != before["login_id"] {
			t.Fatal("失败连接影响旧登录")
		}
	}
	accessProcessResponse(t, healthy, demopb.MsgId_C2S_ENTER, &demopb.EnterRequest{}, &demopb.EnterResponse{})
}
