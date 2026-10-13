package command

import "github.com/cnooljaw/gsr/runtime"

// Probe 是各节点诊断 Service 的命令，不代表业务已经实现。
const Probe runtime.CommandID = 1

// ProbePush 是 access Agent 接收组端诊断通知的内部命令。
const ProbePush runtime.CommandID = 2

// 业务 CommandID 与客户端 msg_id 使用独立编号空间。
const (
	Authenticate runtime.CommandID = 10 + iota
	Position
	Disconnect
	AllocateRoom
	ReserveSeat
	BindRoom
	RemoveMember
	RoomSnapshot
	Hit
	RoomState
	RoomPhase
	Replaced
	Advance
	Sweep
)
