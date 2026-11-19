package observability

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// buildLogger 生成 stdout JSON logger；provider 非 nil 时把同样的记录桥接进 OTel Logs。
// 字段与级别集中在此管理，业务代码不再自行拼装编码器。
func buildLogger(opts Options, provider *sdklog.LoggerProvider) *zap.Logger {
	encoder := zap.NewProductionEncoderConfig()
	encoder.TimeKey = "time"
	encoder.LevelKey = "level"
	encoder.NameKey = "logger"
	encoder.CallerKey = "caller"
	encoder.MessageKey = "msg"
	encoder.StacktraceKey = "stack"
	encoder.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02T15:04:05.000Z07:00")
	encoder.EncodeLevel = zapcore.LowercaseLevelEncoder
	stdout := zapcore.NewCore(zapcore.NewJSONEncoder(encoder),
		zapcore.Lock(os.Stdout), zap.NewAtomicLevelAt(level(opts.LogLevel)))
	cores := []zapcore.Core{stdout}
	if provider != nil {
		// otelzap Core 把 zap 记录送入 OTel Logs；scope 名固定为本包导入路径，
		// 进程维度靠 resource 的 service.name 区分。
		cores = append(cores, otelzap.NewCore("github.com/sunshanpeng/yummy/internal/lib/observability",
			otelzap.WithLoggerProvider(provider)))
	}
	base := zap.New(zapcore.NewTee(cores...), zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
	fields := []zap.Field{zap.String("service.name", opts.ServiceName)}
	if opts.ServiceVersion != "" {
		fields = append(fields, zap.String("service.version", opts.ServiceVersion))
	}
	return base.With(fields...)
}

// level 解析日志级别配置，未知取值回落 info，避免配置笔误导致进程起不来。
func level(name string) zapcore.Level {
	switch strings.ToLower(name) {
	case "debug":
		return zapcore.DebugLevel
	case "warn":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

// LoggerFrom 返回注入了 trace_id/span_id 的 logger，供请求处理与后台任务调用点使用；
// ctx 没有有效 span 时返回基础 logger，不产生空字段噪声。
func (r *Runtime) LoggerFrom(ctx context.Context) *zap.Logger {
	span := trace.SpanContextFromContext(ctx)
	if !span.IsValid() {
		return r.Logger
	}
	return r.Logger.With(
		zap.String("trace_id", span.TraceID().String()),
		zap.String("span_id", span.SpanID().String()),
	)
}
