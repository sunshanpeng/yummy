package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
)

var ErrInvalidPage = errors.New("分页参数或游标无效")

type pageCursor struct {
	Scope string `json:"s"`
	After string `json:"a"`
}

// ParsePage 对游标绑定查询作用域，返回有界 keyset 页大小和最后资源 ID。
func ParsePage(limit int, cursor, scope string) (int, string, error) {
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 || len(cursor) > 2048 {
		return 0, "", ErrInvalidPage
	}
	if cursor == "" {
		return limit, "", nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", ErrInvalidPage
	}
	var value pageCursor
	if err := json.Unmarshal(data, &value); err != nil || value.Scope != scope || value.After == "" || len(value.After) > 128 {
		return 0, "", ErrInvalidPage
	}
	return limit, value.After, nil
}

func NextCursor(scope, after string) string {
	data, _ := json.Marshal(pageCursor{Scope: scope, After: after})
	return base64.RawURLEncoding.EncodeToString(data)
}
