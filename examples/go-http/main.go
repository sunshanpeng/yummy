// 最小 Go HTTP 服务：/ 返回应用信息，/health 供平台探针。
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"service":   "go-http",
			"host":      os.Getenv("HOSTNAME"),
			"time":      time.Now().UTC().Format(time.RFC3339),
			"message":   "Hello from Yummy PaaS",
		})
	})
	// 监听 8080：与平台 Go HTTP 模板默认端口一致。
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		os.Exit(1)
	}
}
