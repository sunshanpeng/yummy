package observability

import (
	"net/http"

	prom "github.com/prometheus/client_golang/prometheus"
	promhttp "github.com/prometheus/client_golang/prometheus/promhttp"
)

// prometheusRegistry 用独立 registry 承载 OTel 导出的指标，
// 不混入进程内其他 client_golang 直接注册的采集器。
func prometheusRegistry() *prom.Registry {
	return prom.NewRegistry()
}

// prometheusHandler 输出 Prometheus 文本格式，供 pull 通道抓取（/metrics）。
func prometheusHandler(registry *prom.Registry) http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
