package model

// RoomBinding 是房间确认的身份与令牌；全为值字段，可直接复制和比较。
type RoomBinding struct {
	Identity             Identity
	RoomID, BindingToken string
}

// Values 新建房间绑定的业务字段集合。
func (binding RoomBinding) Values() map[string]any {
	return map[string]any{"identity": binding.Identity.Values(), "room_id": binding.RoomID, "binding_token": binding.BindingToken}
}

// ReadBinding 校验当前绑定所需的身份、代次和令牌。
func ReadBinding(value any) (RoomBinding, bool) {
	fields, ok := value.(map[string]any)
	if !ok {
		return RoomBinding{}, false
	}
	binding := RoomBinding{}
	binding.Identity, ok = ReadIdentity(fields["identity"])
	if !ok || binding.Identity.LocationVersion == 0 {
		return RoomBinding{}, false
	}
	binding.RoomID, ok = fields["room_id"].(string)
	if !ok || binding.RoomID == "" {
		return RoomBinding{}, false
	}
	binding.BindingToken, ok = fields["binding_token"].(string)
	return binding, ok && binding.BindingToken != ""
}

// SameBinding 防止旧连接、迟到推送和旧房间命令作用到新绑定。
func SameBinding(first, second RoomBinding) bool {
	return first.RoomID != "" && first == second
}

// RoomRequest 冻结一次条件换绑/移除的内容，断线不改变同操作的重试参数。
type RoomRequest struct {
	OperationID string
	Binding     RoomBinding
	Target      Identity
	Connected   bool
}

// RoomReply 的 Snapshot 是新建的业务值集合，接收方拥有它，不含 Room 内部引用。
type RoomReply struct {
	Code     string
	Binding  RoomBinding
	Snapshot map[string]any
}

// Values 组装命令结果；Snapshot 必须是调用方独占的新建集合。
func (reply RoomReply) Values() map[string]any {
	fields := map[string]any{"code": reply.Code}
	if reply.Binding.RoomID != "" {
		fields["binding"] = reply.Binding.Values()
	}
	if reply.Snapshot != nil {
		fields["snapshot"] = reply.Snapshot
	}
	return fields
}

// ReadRoomReply 校验结果外层；游戏快照的字段由使用它的业务出口检查。
func ReadRoomReply(value any) (RoomReply, bool) {
	fields, ok := value.(map[string]any)
	if !ok {
		return RoomReply{}, false
	}
	reply := RoomReply{}
	reply.Code, ok = fields["code"].(string)
	if !ok || reply.Code == "" {
		return RoomReply{}, false
	}
	if value, exists := fields["binding"]; exists {
		reply.Binding, ok = ReadBinding(value)
		if !ok {
			return RoomReply{}, false
		}
	}
	if value, exists := fields["snapshot"]; exists {
		reply.Snapshot, ok = value.(map[string]any)
		if !ok {
			return RoomReply{}, false
		}
	}
	return reply, true
}
