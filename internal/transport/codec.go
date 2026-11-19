package transport

import (
	"errors"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// StrictJSONCodec 不沿用 Connect 默认的 DiscardUnknown，避免托管字段被静默忽略。
// 二进制未知字段另由拦截器拒绝，API/CLI/MCP都不能接受节点/仓库/平台覆盖。
type StrictJSONCodec struct{}

func (StrictJSONCodec) Name() string { return "json" }
func (StrictJSONCodec) Marshal(value any) ([]byte, error) {
	message, ok := value.(proto.Message)
	if !ok {
		return nil, errors.New("无效协议消息")
	}
	return protojson.MarshalOptions{UseProtoNames: false}.Marshal(message)
}
func (StrictJSONCodec) Unmarshal(data []byte, value any) error {
	message, ok := value.(proto.Message)
	if !ok {
		return errors.New("无效协议消息")
	}
	return protojson.UnmarshalOptions{DiscardUnknown: false}.Unmarshal(data, message)
}
