// Package observability 统一封装控制面与 worker 共用的 trace / metric / log 三信号，
// 是领域无关的公共库：SDK 初始化、导出通道与日志编码都集中在这里，业务代码只声明
// 指标名与属性、通过 LoggerFrom 取带 trace 上下文的 logger。
//
//   - 日志：zap JSON 编码输出 stdout，并经 otelzap 桥接进 OTel Logs（OTLP push）；
//     trace_id/span_id 由 LoggerFrom(ctx) 注入，不散落 fmt.Print 与标准库 log。
//   - 指标：同一份 OTel 指标双通道导出——pull（Prometheus /metrics 抓取）与
//     push（OTLP 推 collector），按部署形态可独立启停。
//   - 追踪：OTel TracerProvider（OTLP push），贯穿 HTTP、Connect RPC 与后台任务。
package observability

import (
	"context"
	"errors"
	"net/http"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Options 描述三信号的启动配置。OTLPEndpoint 为空时关闭 push 通道；
// MetricsPull 开启时由调用方把 Runtime.MetricsHandler 挂到自己的路由上。
type Options struct {
	ServiceName    string  // 服务名，如 yummy-control-plane / yummy-worker / yummy-build-runner
	ServiceVersion string  // 构建版本，可为空
	OTLPEndpoint   string  // OTLP HTTP 端点（http:// 前缀自动视为非 TLS），空则关闭 push
	MetricsPull    bool    // 是否启用 Prometheus pull（/metrics）
	TraceRatio     float64 // 采样率 0~1，0 表示全量（默认）
	LogLevel       string  // debug / info / warn / error，默认 info
}

// Runtime 持有三信号的 provider 与对外暴露物，由各入口装配持有、随进程退出 Shutdown。
type Runtime struct {
	Logger         *zap.Logger  // JSON 日志，push 开启时已桥接 OTel Logs
	MetricsHandler http.Handler // Prometheus 抓取入口；未启用 pull 时为 nil

	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	loggerProvider *sdklog.LoggerProvider
	options        Options
}

// Start 按配置初始化三信号。任何导出器创建失败都会返回错误，调用方不应带病启动。
func Start(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.ServiceName == "" {
		opts.ServiceName = "yummy"
	}
	if opts.TraceRatio <= 0 {
		opts.TraceRatio = 1
	}
	r := &Runtime{options: opts}

	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(
			attribute.String("service.name", opts.ServiceName),
			attribute.String("service.version", opts.ServiceVersion),
		))
	if err != nil {
		return nil, err
	}

	// 追踪：provider 始终创建（无导出器时 span 只是被丢弃），保证调用侧拿到的类型稳定；
	// push 开启时追加 OTLP 批量导出。
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(opts.TraceRatio))),
	)
	r.tracerProvider = tp
	if opts.OTLPEndpoint != "" {
		spanExporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(opts.OTLPEndpoint))
		if err != nil {
			return nil, err
		}
		tp.RegisterSpanProcessor(sdktrace.NewBatchSpanProcessor(spanExporter))
	}

	// 指标：pull 与 push 两个 reader 互不依赖，可只开其一；reader 只能在构造时传入。
	metricOptions := []sdkmetric.Option{sdkmetric.WithResource(res)}
	if opts.MetricsPull {
		registry := prometheusRegistry()
		exporter, err := otelprometheus.New(otelprometheus.WithRegisterer(registry))
		if err != nil {
			return nil, err
		}
		metricOptions = append(metricOptions, sdkmetric.WithReader(exporter))
		r.MetricsHandler = prometheusHandler(registry)
	}
	if opts.OTLPEndpoint != "" {
		metricExporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(opts.OTLPEndpoint))
		if err != nil {
			return nil, err
		}
		metricOptions = append(metricOptions, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)))
	}
	r.meterProvider = sdkmetric.NewMeterProvider(metricOptions...)

	// 日志：stdout JSON 之外，push 开启时再挂一条 OTel Logs 通道（见 logger.go）。
	logOptions := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	if opts.OTLPEndpoint != "" {
		logExporter, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(opts.OTLPEndpoint))
		if err != nil {
			return nil, err
		}
		logOptions = append(logOptions, sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)))
	}
	r.loggerProvider = sdklog.NewLoggerProvider(logOptions...)
	r.Logger = buildLogger(opts, r.loggerProvider)

	// 服务版本允许来自构建期注入的环境变量，未设置时字段省略，避免噪声。
	if opts.ServiceVersion == "" && os.Getenv("YUMMY_VERSION") != "" {
		r.Logger = r.Logger.With(zap.String("service.version", os.Getenv("YUMMY_VERSION")))
	}
	return r, nil
}

// Shutdown 依次冲刷并关闭三信号 provider，进程退出前调用。
func (r *Runtime) Shutdown(ctx context.Context) error {
	var errs []error
	if r.loggerProvider != nil {
		errs = append(errs, r.loggerProvider.Shutdown(ctx))
	}
	if r.tracerProvider != nil {
		errs = append(errs, r.tracerProvider.Shutdown(ctx))
	}
	if r.meterProvider != nil {
		errs = append(errs, r.meterProvider.ForceFlush(ctx))
		errs = append(errs, r.meterProvider.Shutdown(ctx))
	}
	if r.Logger != nil {
		_ = r.Logger.Sync()
	}
	return errors.Join(errs...)
}

// Tracer 返回带服务名的 tracer，供后台任务与关键链路手动埋 span。
func (r *Runtime) Tracer(name string) oteltrace.Tracer {
	return r.tracerProvider.Tracer(name)
}

// Meter 返回带服务名的 meter，供业务域声明指标。
func (r *Runtime) Meter(name string) otelmetric.Meter {
	return r.meterProvider.Meter(name)
}
