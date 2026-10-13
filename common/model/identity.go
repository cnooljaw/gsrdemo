package model

// Identity 是接入身份的值拷贝；位置代次由 Online 分配。
type Identity struct {
	UserID, LoginID, AccessNode, AgentName string
	LocationVersion                        uint64
}

// Values 只组装业务字段，传输编码由 GSR 的默认 ValueCodec 完成。
func (identity Identity) Values() map[string]any {
	return map[string]any{
		"user_id": identity.UserID, "login_id": identity.LoginID,
		"access_node": identity.AccessNode, "agent_name": identity.AgentName,
		"location_version": identity.LocationVersion,
	}
}

// ReadIdentity 校验跨节点身份字段，不接受数字类型隐式转换。
func ReadIdentity(value any) (Identity, bool) {
	fields, ok := value.(map[string]any)
	if !ok {
		return Identity{}, false
	}
	identity := Identity{}
	identity.UserID, ok = fields["user_id"].(string)
	if !ok || identity.UserID == "" {
		return Identity{}, false
	}
	identity.LoginID, ok = fields["login_id"].(string)
	if !ok || identity.LoginID == "" {
		return Identity{}, false
	}
	identity.AccessNode, ok = fields["access_node"].(string)
	if !ok || identity.AccessNode == "" {
		return Identity{}, false
	}
	identity.AgentName, ok = fields["agent_name"].(string)
	if !ok || identity.AgentName == "" {
		return Identity{}, false
	}
	identity.LocationVersion, ok = fields["location_version"].(uint64)
	return identity, ok
}

// SameIdentity 比较服务端身份及其确认的接入代次。
func SameIdentity(first, second Identity) bool {
	return first.UserID != "" && first == second
}
