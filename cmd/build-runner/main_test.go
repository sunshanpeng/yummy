package main

import (
	"context"
	"testing"
)

func TestRunnerRejectsUnsupportedCLI(t *testing.T) {
	// runner 从环境变量读取 OTLP 端点；开发机常设置该变量，每次 run 退出时都会等满导出超时，
	// 测试因此变慢且依赖外部环境，这里显式清空。
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	for _, args := range [][]string{nil, {"shell"}, {"build", "--metadata-output=/host/file", "--", "build"}, {"build", "--unknown"}, {"build", "--", "build", "--allow=network.host"}, {"prepare", "--bad-flag"}, {"prepare", "extra"}} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("未拒绝%v", args)
		}
	}
}
