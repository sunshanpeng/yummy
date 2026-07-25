package storage

import (
	"context"
	"testing"
)

func TestOpenRejectsMissingConfiguration(t *testing.T) {
	_, err := Open(context.Background(), Config{})
	if err == nil {
		t.Fatal("空配置必须返回错误")
	}
}
