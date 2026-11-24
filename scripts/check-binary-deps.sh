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

# runner 镜像的构建阶段只 COPY 固定目录。runner 依赖的本模块包若不在其中，
# 要到镜像流水线才会构建失败（2026-11-19 runner 接入 observability 后曾发生），这里提前检查。
check_runner_image_inputs() {
  local dockerfile=deploy/kubernetes/runner.Dockerfile module packages copied directory entry covered missing=""
  module=$(go list -m)
  copied=$(awk '$1 == "COPY" && $2 !~ /^--/ { print $2 }' "$dockerfile")
  packages=$(go list -deps -f '{{.ImportPath}}' ./cmd/build-runner | awk -v module="$module" 'index($0, module "/") == 1 { print substr($0, length(module) + 2) }')
  while IFS= read -r directory; do
    covered=false
    while IFS= read -r entry; do
      entry=${entry#./}
      entry=${entry%/}
      if [[ -n "$entry" && ( "$directory" == "$entry" || "$directory" == "$entry"/* ) ]]; then
        covered=true
        break
      fi
    done <<< "$copied"
    if [[ "$covered" != true ]]; then
      missing+="  $directory"$'\n'
    fi
  done <<< "$packages"
  if [[ -n "$missing" ]]; then
    printf '%s 缺少 build-runner 依赖的目录:\n%s' "$dockerfile" "$missing" >&2
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
# build-runner 只在构建 Pod 内运行：不带数据库、集群客户端和任何入口层依赖。
check_dependencies ./cmd/build-runner "$database|$kubernetes|$server|$worker|/internal/transport/clientcli(/|$)"
check_runner_image_inputs
