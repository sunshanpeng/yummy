package kube

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func metricText(cpu, start, memory float64) []byte {
	return []byte(fmt.Sprintf(`# TYPE container_cpu_usage_seconds_total counter
container_cpu_usage_seconds_total{namespace="runtime",pod="app-pod",container="app"} %g
# TYPE container_start_time_seconds gauge
container_start_time_seconds{namespace="runtime",pod="app-pod",container="app"} %g
# TYPE container_memory_working_set_bytes gauge
container_memory_working_set_bytes{namespace="runtime",pod="app-pod",container="app"} %g
container_memory_working_set_bytes{namespace="foreign",pod="foreign",container="app"} 123
`, cpu, start, memory))
}
func TestMetricsActualIntervalStartupAndMissing(t *testing.T) {
	a, client, _ := fixture(t)
	c := change(a)
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: "runtime", UID: "pod-uid", Labels: runtimeLabels(c)}, Spec: corev1.PodSpec{NodeName: a.config.Node.Name}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	_ = client.Tracker().Add(p)
	m := &MetricsCollector{adapter: a, previous: map[metricIdentity]cpuPoint{}}
	at := time.Unix(1000, 0)
	first, err := m.samples(ctx, metricText(100, 10, 1024), at)
	if err != nil || len(first) != 1 || first[0].CPUCores != nil || *first[0].MemoryWorkingSetBytes != 1024 || first[0].PodUID != p.UID {
		t.Fatal(first, err)
	}
	next, err := m.samples(ctx, metricText(115, 10, 2048), at.Add(45*time.Second))
	if err != nil || next[0].CPUCores == nil || *next[0].CPUCores != 1.0/3 {
		t.Fatal(next, err)
	}
	reset, err := m.samples(ctx, metricText(1, 10, 2048), at.Add(time.Minute))
	if err != nil || reset[0].CPUCores != nil {
		t.Fatal(reset, err)
	}
	startup, err := m.samples(ctx, metricText(30, 20, 2048), at.Add(90*time.Second))
	if err != nil || startup[0].CPUCores != nil {
		t.Fatal(startup, err)
	}
	missing, err := m.samples(ctx, []byte(""), at.Add(2*time.Minute))
	if err != nil || len(missing) != 0 {
		t.Fatal(missing, err)
	}
	resumed, err := m.samples(ctx, metricText(60, 20, 2048), at.Add(150*time.Second))
	if err != nil || resumed[0].CPUCores != nil {
		t.Fatal(resumed, err)
	}
	if _, err := m.samples(ctx, []byte("malformed {"), at); err == nil {
		t.Fatal("无效Prometheus文本未拒绝")
	}
}
func TestMetricsTLSAndAuthorizationNoRedirect(t *testing.T) {
	a, client, _ := fixture(t)
	c := change(a)
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: "runtime", UID: "pod-uid", Labels: runtimeLabels(c)}, Spec: corev1.PodSpec{NodeName: a.config.Node.Name}}
	_ = client.Tracker().Add(p)
	redirect := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics/resource" || r.Header.Get("Authorization") != "Bearer restricted" {
			t.Error("请求不符合nodes/metrics契约")
		}
		if redirect {
			http.Redirect(w, r, "https://other.example/metrics/resource", http.StatusFound)
			return
		}
		_, _ = w.Write(metricText(10, 1, 42))
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	m, err := a.Metrics(MetricsConfig{Endpoint: server.URL + "/metrics/resource", CAData: ca, BearerToken: "restricted"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	samples, err := m.Collect(ctx)
	if err != nil || len(samples) != 1 {
		t.Fatal(samples, err)
	}
	redirect = true
	if _, err := m.Collect(ctx); err == nil {
		t.Fatal("不允许携带令牌重定向")
	}
	for _, endpoint := range []string{strings.Replace(server.URL, "https:", "http:", 1) + "/metrics/resource", server.URL + "/metrics/resource?token=x", server.URL + "/api/v1/nodes/proxy"} {
		if _, err := a.Metrics(MetricsConfig{Endpoint: endpoint, CAData: ca, BearerToken: "restricted"}); err == nil {
			t.Fatal("不安全metrics URL未拒绝")
		}
	}
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer other.Close()
	badCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: other.Certificate().Raw})
	// httptest默认共享测试证书；使用无关根证书测试由另一个测试生成器覆盖。这里明确检查未提供CA必拒绝。
	_ = badCA
	if _, err := a.Metrics(MetricsConfig{Endpoint: server.URL + "/metrics/resource", BearerToken: "restricted"}); err == nil {
		t.Fatal("缺少CA未拒绝")
	}
}
