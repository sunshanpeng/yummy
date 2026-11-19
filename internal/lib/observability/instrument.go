package observability

import (
	"net/http"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	otelhttp "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"
)

// HTTPHandler 用 otelhttp 包装 HTTP 处理器：自动记录 server span 与 HTTP 指标，
// 并保证 trace 上下文按 W3C 头传播。装配层用它包住最外层 mux（含 gin）。
func (r *Runtime) HTTPHandler(name string, handler http.Handler) http.Handler {
	return otelhttp.NewHandler(handler, name,
		otelhttp.WithTracerProvider(r.tracerProvider),
		otelhttp.WithMeterProvider(r.meterProvider))
}

// ConnectOptions 返回 Connect handler 的可观测性选项：otelconnect 拦截器提供
// RPC server span、RPC 指标与 trace 上下文传播。装配层把它附加到每个 Connect 服务，
// 客户端侧（worker 调控制面）也用同一拦截器保持链路贯穿。
func (r *Runtime) ConnectOptions() []connect.HandlerOption {
	interceptor, err := otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(r.tracerProvider),
		otelconnect.WithMeterProvider(r.meterProvider),
	)
	if err != nil {
		r.Logger.Warn("otelconnect 拦截器创建失败，RPC 可观测性降级", zap.Error(err))
		return nil
	}
	return []connect.HandlerOption{connect.WithInterceptors(interceptor)}
}

// ConnectClientOptions 返回 Connect 客户端侧选项（worker 连控制面等出站调用），
// 与 handler 侧同一 otelconnect 拦截器，保持跨进程 trace 贯穿与 RPC 指标。
func (r *Runtime) ConnectClientOptions() []connect.ClientOption {
	interceptor, err := otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(r.tracerProvider),
		otelconnect.WithMeterProvider(r.meterProvider),
	)
	if err != nil {
		r.Logger.Warn("otelconnect 客户端拦截器创建失败，RPC 可观测性降级", zap.Error(err))
		return nil
	}
	return []connect.ClientOption{connect.WithInterceptors(interceptor)}
}
