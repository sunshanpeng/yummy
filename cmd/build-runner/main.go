// 固定runner仅由构建Pod调用，不是worker的任意命令执行API。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sunshanpeng/yummy/internal/platform/buildrunner"
)

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("需要固定prepare/build子命令")
	}
	switch args[0] {
	case "prepare":
		flags := flag.NewFlagSet("prepare", flag.ContinueOnError)
		uri := flags.String("source-uri", "", "")
		token := flags.String("grant-file", "", "")
		digest := flags.String("archive-sha256", "", "")
		max := flags.Int64("max-bytes", 0, "")
		destination := flags.String("destination", "", "")
		dockerfile := flags.String("dockerfile", "", "")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("prepare不接受额外参数")
		}
		return buildrunner.PrepareSource(ctx, buildrunner.PrepareOptions{URI: *uri, TokenFile: *token, ArchiveSHA256: *digest, Destination: *destination, Dockerfile: *dockerfile, MaxBytes: *max})
	case "build":
		flags := flag.NewFlagSet("build", flag.ContinueOnError)
		sandbox := flags.Bool("no-process-sandbox", false, "")
		metadata := flags.String("metadata-output", "/dev/termination-log", "")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *metadata != "/dev/termination-log" {
			return fmt.Errorf("结果路径固定")
		}
		return buildrunner.RunBuildKit(ctx, flags.Args(), *sandbox, os.Stdout, os.Stderr)
	default:
		return fmt.Errorf("未知runner子命令")
	}
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
