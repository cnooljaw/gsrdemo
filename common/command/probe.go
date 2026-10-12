package command

import "github.com/cnooljaw/gsr/runtime"

// Probe 是各节点诊断 Service 的命令，不代表业务已经实现。
const Probe runtime.CommandID = 1

// ProbePush 是 access Agent 接收组端诊断通知的内部命令。
const ProbePush runtime.CommandID = 2
