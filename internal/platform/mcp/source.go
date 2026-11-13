package mcp

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sunshanpeng/yummy/internal/platform/authorization"
)

type snapshotView struct {
	ID              string    `json:"id"`
	State           string    `json:"state"`
	SHA256          string    `json:"sha256"`
	CompressedBytes int64     `json:"compressedBytes"`
	ExpandedBytes   int64     `json:"expandedBytes"`
	FileBytes       int64     `json:"fileBytes"`
	EntryCount      int64     `json:"entryCount"`
	Dockerfile      string    `json:"dockerfile"`
	CreatedAt       time.Time `json:"createdAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

func registerSourceTools(server *sdk.Server, s scope) {
	typedTool(server, "source_snapshot_get", "读取当前应用已上传源码快照的摘要；不读取原始源码或主机文件。", annotations(true, false, true, false), func(ctx context.Context, in idInput) (any, *sdk.CallToolResult) {
		if err := s.check(authorization.SourceRead); err != nil {
			return nil, err
		}
		if !validID(in.ID) {
			return nil, toolError("INVALID_ARGUMENT", "快照 ID 无效")
		}
		v, err := s.services.Sources.GetSnapshot(ctx, s.principal, s.workspaceID, s.applicationID, in.ID)
		if err != nil {
			return nil, failure(err)
		}
		return snapshotView{v.ID, v.State, v.SHA256, v.CompressedBytes, v.ExpandedBytes, v.FileBytes, v.EntryCount, v.Dockerfile, v.CreatedAt, v.ExpiresAt}, nil
	})
}
