// 固定runner仅由构建Pod调用，不是worker的任意命令执行API。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sunshanpeng/yummy/internal/infra/buildrunner"
	"github.com/sunshanpeng/yummy/internal/lib/observability"
	"go.uber.org/zap"
)

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("需要固定prepare/build子命令")
	}
	// 一次性进程同样接入三信号：OTLP 端点走标准环境变量 OTEL_EXPORTER_OTLP_ENDPOINT，
	// 未设置时只输出 stdout JSON 日志，不影响构建 Pod 的最小镜像约束。
	obs, err := observability.Start(ctx, observability.Options{
		ServiceName:  "yummy-build-runner",
		OTLPEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	})
	if err != nil {
		return err
	}
	// 退出时冲刷遥测；超时 ctx 在退出时创建（同 serve/worker 的生命周期约束）。
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutdownCtx)
	}()
	logger := obs.LoggerFrom(ctx)
	var runErr error
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
		runErr = buildrunner.PrepareSource(ctx, buildrunner.PrepareOptions{URI: *uri, TokenFile: *token, ArchiveSHA256: *digest, Destination: *destination, Dockerfile: *dockerfile, MaxBytes: *max})
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
		runErr = buildrunner.RunBuildKit(ctx, flags.Args(), *sandbox, os.Stdout, os.Stderr)
	default:
		return fmt.Errorf("未知runner子命令")
	}
	if runErr != nil {
		logger.Error("构建runner子命令失败", zap.String("subcommand", args[0]), zap.Error(runErr))
	}
	return runErr
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
