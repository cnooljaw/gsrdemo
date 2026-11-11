package main

import (
	"encoding/json"

	demopb "github.com/cnooljaw/gsrdemo/nodes/access/gen/demo/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// clientMessage 仅在客户端协议边界转换已有内部快照，不改变 Cluster 参数和类型。
// encoding/json 直接写整数十进制，protojson 直接解析到 int64/uint64，不经过 float64。
func clientMessage(value any, message proto.Message) error {
	if _, ok := value.(map[string]any); !ok {
		return errInvalidBackendReply
	}
	contents, err := json.Marshal(value)
	if err != nil {
		return errInvalidBackendReply
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(contents, message); err != nil {
		return errInvalidBackendReply
	}
	return nil
}

func clientSnapshot(value any) (*demopb.Snapshot, error) {
	snapshot := &demopb.Snapshot{}
	if err := clientMessage(value, snapshot); err != nil {
		return nil, err
	}
	if snapshot.AttackName == "" {
		return nil, errInvalidBackendReply
	}
	return snapshot, nil
}

func clientHit(value any) (*demopb.Hit, error) {
	hit := &demopb.Hit{}
	if err := clientMessage(value, hit); err != nil {
		return nil, err
	}
	if hit.HitId == "" || hit.ActionId == "" || (hit.Status != "accepted" && hit.Status != "rejected") {
		return nil, errInvalidBackendReply
	}
	return hit, nil
}

func clientBalance(value any) (*demopb.Balance, error) {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, errInvalidBackendReply
	}
	if _, ok := fields["balance"].(int64); !ok {
		return nil, errInvalidBackendReply
	}
	balance := &demopb.Balance{}
	if err := clientMessage(value, balance); err != nil {
		return nil, err
	}
	if amount, exists := fields["confirmed_balance"]; exists {
		confirmed, amountOK := amount.(int64)
		version, versionOK := fields["version"].(uint64)
		if !amountOK || !versionOK {
			return nil, errInvalidBackendReply
		}
		balance.Confirmed = &demopb.ConfirmedSilver{Amount: confirmed, Version: version}
	}
	return balance, nil
}
