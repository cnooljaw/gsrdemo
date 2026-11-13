package main

import (
	"errors"

	"github.com/cnooljaw/gsr"
	"github.com/cnooljaw/gsr/protocol"
	"github.com/cnooljaw/gsr/runtime"
	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
)

// unknownEnterError 保留原错误，仅通知 Agent 当前进入结果需要断线收口。
type unknownEnterError struct{ error }

func (err *unknownEnterError) Unwrap() error { return err.error }

func enterFailure(err error) error {
	if enterOutcomeUnknown(err) {
		return &unknownEnterError{err}
	}
	return err
}

// enterOutcomeUnknown 仅用于进入流程：业务拒绝、明确未接纳或目标已消失不需要断线，
// 超时、链路故障或未分类 Handler 失败都不能证明后端没有加入。
func enterOutcomeUnknown(err error) bool {
	var status *runtime.StatusError
	if errors.As(err, &status) && status.Class == runtime.ErrorClassDomain {
		return false
	}
	for _, rejected := range []error{
		runtime.ErrServiceNotFound, runtime.ErrServiceStarting, runtime.ErrServiceStopping,
		runtime.ErrServiceClosed, runtime.ErrServicePanicked, runtime.ErrMailboxFull,
		runtime.ErrUnknownCommand, runtime.ErrInvalidCommandArguments,
	} {
		if errors.Is(err, rejected) {
			return false
		}
	}
	return err != nil
}

// clientErrorPolicy 在 Dispatcher 自动回错前封闭后续业务；发送和关闭由 Gate 完成。
func (agent *agent) clientErrorPolicy(_ gsr.CommandContext, msgID uint32, cause error) protocol.ErrorAction {
	loginRequest := msgID == uint32(demopb.MsgId_C2S_LOGIN) || msgID == uint32(demopb.MsgId_C2S_PASSWORD_AUTH)
	var unknownEnter *unknownEnterError
	if loginRequest || errors.Is(cause, errInvalidBackendReply) || errors.As(cause, &unknownEnter) {
		agent.closing = true
		return protocol.ReplyErrorAndClose
	}
	return protocol.ReplyError
}
