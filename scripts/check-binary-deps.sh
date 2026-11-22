#!/usr/bin/env bash
set -euo pipefail

check_dependencies() {
  local entry=$1 forbidden=$2 dependencies matches
  dependencies=$(go list -deps "$entry")
  matches=$(awk -v pattern="$forbidden" '$0 ~ pattern' <<< "$dependencies")
  if [[ -n "$matches" ]]; then
    printf '%s 包含超出分发边界的依赖\n%s\n' "$entry" "$matches" >&2
    return 1
  fi
}

database='^gorm[.]io/|^github[.]com/jackc/pgx/|/internal/infra/store(/|$)'
kubernetes='^k8s[.]io/client-go/|/internal/infra/(kube|workerexec)(/|$)'
server='/internal/(controlplane|domains|bff)(/|$)|/internal/transport/workergateway(/|$)'
worker='/internal/transport/(workercli|workerclient)(/|$)'

check_dependencies ./cmd/yummy "$database|$kubernetes|$server|$worker"
check_dependencies ./cmd/yummy-server "$kubernetes|$worker|/internal/transport/clientcli(/|$)"
check_dependencies ./cmd/yummy-worker "$database|$server|/internal/transport/clientcli(/|$)"
