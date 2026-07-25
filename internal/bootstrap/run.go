package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"time"
)

func Run(ctx context.Context, mode string, args []string, assets fs.FS) error {
	config, err := ParseFlags(ConfigFromEnvironment(), args, os.Stdout)
	if err != nil {
		return err
	}
	switch mode {
	case "all":
		return RunAll(ctx, config, assets)
	case "control":
		return RunControl(ctx, config, assets)
	case "worker":
		return RunWorker(ctx, config)
	default:
		return fmt.Errorf("未知运行模式 %q", mode)
	}
}

func newID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err == nil {
		return hex.EncodeToString(data)
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}
