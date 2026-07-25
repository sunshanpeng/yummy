#!/usr/bin/env sh
set -eu

mkdir -p var
pids=""

cleanup() {
  for pid in $pids; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in $pids; do
    wait "$pid" 2>/dev/null || true
  done
  pids=""
}

trap cleanup EXIT INT TERM

start_process() {
  "$@" &
  pids="$! $pids"
}

wait_for_text() {
  url=$1
  data=$2
  expected=$3
  attempts=0
  while [ "$attempts" -lt 80 ]; do
    if curl -fsS -H "Content-Type: application/json" --data "$data" "$url" 2>/dev/null | grep -q "$expected"; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 0.1
  done
  echo "等待 $url 出现 $expected 超时" >&2
  return 1
}

assert_uppercase() {
  base_url=$1
  worker_id=$2
  input=$3
  expected=$4
  response=$(curl -fsS \
    -H "Content-Type: application/json" \
    --data "{\"workerId\":\"$worker_id\",\"input\":\"$input\"}" \
    "$base_url/api/demo.v1.ControlService/CreateCommand")
  command_id=$(printf "%s" "$response" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  if [ -z "$command_id" ]; then
    echo "创建指令响应缺少 id: $response" >&2
    return 1
  fi

  attempts=0
  while [ "$attempts" -lt 80 ]; do
    response=$(curl -fsS \
      -H "Content-Type: application/json" \
      --data "{\"id\":\"$command_id\"}" \
      "$base_url/api/demo.v1.ControlService/GetCommand")
    if printf "%s" "$response" | grep -q "\"output\":\"$expected\""; then
      return 0
    fi
    if printf "%s" "$response" | grep -q "FAILED"; then
      echo "指令执行失败: $response" >&2
      return 1
    fi
    attempts=$((attempts + 1))
    sleep 0.1
  done
  echo "等待指令 $command_id 完成超时" >&2
  return 1
}

start_process env PUBLIC_ADDR=127.0.0.1:19080 ./bin/platform
wait_for_text \
  "http://127.0.0.1:19080/api/demo.v1.ControlService/ListWorkers" \
  "{}" \
  "local-worker"
assert_uppercase "http://127.0.0.1:19080" "local-worker" "single mode" "SINGLE MODE"
echo "PASS 单进程：platform 默认 all"
cleanup

start_process env PUBLIC_ADDR=127.0.0.1:19081 ./bin/platform all
start_process env \
  VITE_CONTROL_PROXY=http://127.0.0.1:19081 \
  pnpm --dir web exec vite --host 127.0.0.1 --port 15173
wait_for_text \
  "http://127.0.0.1:15173/api/demo.v1.ControlService/ListWorkers" \
  "{}" \
  "local-worker"
assert_uppercase "http://127.0.0.1:15173" "local-worker" "split frontend" "SPLIT FRONTEND"
echo "PASS 前后端分离：Vite 代理独立 Go 后端"
cleanup

start_process env \
  PUBLIC_ADDR=127.0.0.1:19082 \
  AGENT_ADDR=127.0.0.1:19443 \
  CERT_DIR=var/certs \
  ./bin/control
start_process env \
  CONTROL_URL=https://127.0.0.1:19443 \
  TLS_SERVER_NAME=localhost \
  CERT_DIR=var/certs \
  WORKER_ID=worker-1 \
  ./bin/worker --heartbeat-interval 1s
wait_for_text \
  "http://127.0.0.1:19082/api/demo.v1.ControlService/ListWorkers" \
  "{}" \
  "worker-1"
assert_uppercase "http://127.0.0.1:19082" "worker-1" "remote worker" "REMOTE WORKER"
echo "PASS 控制面/worker 分离：mTLS ConnectRPC 双向流"
cleanup
