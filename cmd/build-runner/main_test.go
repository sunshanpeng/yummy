package main

import (
	"context"
	"testing"
)

func TestRunnerRejectsUnsupportedCLI(t *testing.T) {
	for _, args := range [][]string{nil, {"shell"}, {"build", "--metadata-output=/host/file", "--", "build"}, {"build", "--unknown"}, {"build", "--", "build", "--allow=network.host"}, {"prepare", "--bad-flag"}, {"prepare", "extra"}} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("未拒绝%v", args)
		}
	}
}
