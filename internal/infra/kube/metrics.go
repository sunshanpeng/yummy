package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/expfmt"
	prommodel "github.com/prometheus/common/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

// MetricsConfig 由安装者显式配置本节点Kubelet的证书、URL及nodes/metrics凭证。
// 不读取kubeconfig，不走nodes/proxy，不允许跳过TLS验证或重定向携带凭证。
type MetricsConfig struct {
	Endpoint                             string
	CAData, ClientCertificate, ClientKey []byte
	BearerToken                          string
}
type MetricsCollector struct {
	adapter         *Adapter
	client          *http.Client
	endpoint, token string
	mu              sync.Mutex
	previous        map[metricIdentity]cpuPoint
}
type metricIdentity struct {
	podUID    types.UID
	container string
}
type cpuPoint struct {
	value, start float64
	at           time.Time
}
type MetricSample struct {
	CollectedAt                           time.Time
	WorkspaceID, ApplicationID, ReleaseID string
	PodUID                                types.UID
	Container                             string
	ContainerStartSeconds                 *float64
	CPUCores                              *float64
	MemoryWorkingSetBytes                 *int64
}

func (a *Adapter) Metrics(config MetricsConfig) (*MetricsCollector, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Path != "/metrics/resource" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || len(config.CAData) == 0 {
		return nil, fmt.Errorf("%w: 必须显式配置本node kubelet HTTPS/metrics/resource与CA", ErrInvalid)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(config.CAData) {
		return nil, fmt.Errorf("%w: kubelet CA", ErrInvalid)
	}
	tlsConfig := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	if len(config.ClientCertificate) > 0 || len(config.ClientKey) > 0 {
		cert, err := tls.X509KeyPair(config.ClientCertificate, config.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("%w: kubelet客户端证书", ErrInvalid)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	if config.BearerToken == "" && len(tlsConfig.Certificates) == 0 {
		return nil, fmt.Errorf("%w: 缺少nodes/metrics受限凭证", ErrInvalid)
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil, ResponseHeaderTimeout: 10 * time.Second}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &MetricsCollector{adapter: a, client: client, endpoint: config.Endpoint, token: config.BearerToken, previous: map[metricIdentity]cpuPoint{}}, nil
}
func (m *MetricsCollector) Close() { m.client.CloseIdleConnections() }
func (m *MetricsCollector) Collect(ctx context.Context) ([]MetricSample, error) {
	if err := m.adapter.capability("metrics"); err != nil {
		return nil, err
	}
	if err := m.adapter.checkNode(ctx, m.adapter.config.Node); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.endpoint, nil)
	if err != nil {
		return nil, err
	}
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	response, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kubelet metrics响应HTTP%d", response.StatusCode)
	}
	const maxBytes = 8 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("kubelet metrics超出8MiB边界")
	}
	return m.samples(ctx, data, time.Now().UTC())
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

type scrapeKey struct{ namespace, pod, container string }
type resourceValues struct{ cpu, memory, start *float64 }

func (m *MetricsCollector) samples(ctx context.Context, data []byte, at time.Time) ([]MetricSample, error) {
	parser := expfmt.NewTextParser(prommodel.LegacyValidation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("kubelet Prometheus文本无效: %w", err)
	}
	values := map[scrapeKey]*resourceValues{}
	for _, name := range []string{"container_cpu_usage_seconds_total", "container_memory_working_set_bytes", "container_start_time_seconds"} {
		family := families[name]
		if family == nil {
			continue
		}
		for _, metric := range family.Metric {
			key := scrapeKey{}
			for _, label := range metric.Label {
				switch label.GetName() {
				case "namespace":
					key.namespace = label.GetValue()
				case "pod":
					key.pod = label.GetValue()
				case "container":
					key.container = label.GetValue()
				}
			}
			// 命名空间按空间派生（prod-<slug>，2026-10-26 设计）：按前缀匹配本环境
			// 的运行命名空间；旧固定命名空间保留兼容。
			if !strings.HasPrefix(key.namespace, EnvProd+"-") && key.namespace != m.adapter.config.RuntimeNamespace || key.pod == "" || key.container != "app" {
				continue
			}
			var value float64
			if name == "container_cpu_usage_seconds_total" {
				if metric.Counter == nil {
					continue
				}
				value = metric.Counter.GetValue()
			} else {
				if metric.Gauge == nil {
					continue
				}
				value = metric.Gauge.GetValue()
			}
			if !finite(value) {
				continue
			}
			entry := values[key]
			if entry == nil {
				entry = &resourceValues{}
				values[key] = entry
			}
			switch name {
			case "container_cpu_usage_seconds_total":
				if entry.cpu != nil {
					return nil, fmt.Errorf("重复CPU指标身份")
				}
				entry.cpu = ptr(value)
			case "container_memory_working_set_bytes":
				if entry.memory != nil {
					return nil, fmt.Errorf("重复内存指标身份")
				}
				entry.memory = ptr(value)
			case "container_start_time_seconds":
				if entry.start != nil {
					return nil, fmt.Errorf("重复启动指标身份")
				}
				entry.start = ptr(value)
			}
		}
	}
	// 受管 pod 可能分布在按空间派生的多个运行命名空间：按平台标签集群级列出。
	pods, err := m.adapter.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(map[string]string{LabelManaged: "worker", LabelNodeUID: string(m.adapter.config.Node.UID)}).String()})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := map[metricIdentity]cpuPoint{}
	result := []MetricSample{}
	for _, pod := range pods.Items {
		if pod.UID == "" || pod.Spec.NodeName != m.adapter.config.Node.Name || terminalPod(&pod) || !validIdentity(pod.Labels[LabelApplication]) || !validIdentity(pod.Labels[LabelRelease]) || !validIdentity(pod.Labels[LabelWorkspace]) {
			continue
		}
		entry := values[scrapeKey{pod.Namespace, pod.Name, "app"}]
		if entry == nil {
			continue
		}
		sample := MetricSample{CollectedAt: at, WorkspaceID: pod.Labels[LabelWorkspace], ApplicationID: pod.Labels[LabelApplication], ReleaseID: pod.Labels[LabelRelease], PodUID: pod.UID, Container: "app", ContainerStartSeconds: entry.start}
		if entry.memory != nil && *entry.memory < float64(math.MaxInt64) {
			sample.MemoryWorkingSetBytes = ptr(int64(*entry.memory))
		}
		key := metricIdentity{pod.UID, "app"}
		if entry.cpu != nil && entry.start != nil && *entry.start > 0 {
			point := cpuPoint{value: *entry.cpu, start: *entry.start, at: at}
			previous, ok := m.previous[key]
			elapsed := at.Sub(previous.at).Seconds()
			if ok && previous.start == point.start && point.value >= previous.value && elapsed > 0 {
				cores := (point.value - previous.value) / elapsed
				if finite(cores) {
					sample.CPUCores = &cores
				}
			}
			next[key] = point
		}
		result = append(result, sample)
	}
	// 失败/缺测/容器消失不补0；缺少连续身份或counter重置后重新建立基线。
	m.previous = next
	return result, nil
}
