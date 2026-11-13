#!/usr/bin/env bash
# 产品冒烟：对构建产物做最小真实链路验证（迁移→首位管理员→启动→登录→创建空间→OAuth 元数据）。
# 使用临时数据库 yummy_smoke，结束后删除；需要本机 PostgreSQL/Redis（docker compose 基础设施）。
set -eu

container=${YUMMY_SMOKE_PG_CONTAINER:-yummy-postgres-1}
database=${YUMMY_SMOKE_PG_DATABASE:-yummy_smoke}
dsn_user=${YUMMY_SMOKE_PG_USER:-demo}
public_addr=${YUMMY_SMOKE_PUBLIC_ADDR:-127.0.0.1:18080}
transfer_addr=${YUMMY_SMOKE_TRANSFER_ADDR:-127.0.0.1:18081}
base_url="http://127.0.0.1:18080"
username="smoke-admin"
password="smoke-only-password-000"

platform_bin=${PLATFORM_BIN:-bin/platform}
test -x "$platform_bin" || { echo "缺少 ${platform_bin}，请先 make build" >&2; exit 1; }
dsn="postgres://$dsn_user:$dsn_user@localhost:5432/$database?sslmode=disable"

cleanup() {
  if [ -n "${server_pid:-}" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  docker exec "$container" psql -U "$dsn_user" -d postgres -c "DROP DATABASE IF EXISTS $database" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

session_key=$(openssl rand -base64 32)
csrf_key=$(openssl rand -base64 32)
master_key=$(openssl rand -base64 32)
oauth_key=$(openssl rand -base64 32)

docker exec "$container" psql -U "$dsn_user" -d postgres -c "DROP DATABASE IF EXISTS $database" >/dev/null
docker exec "$container" psql -U "$dsn_user" -d postgres -c "CREATE DATABASE $database OWNER $dsn_user" >/dev/null

"$platform_bin" migrate --database-url "$dsn" | grep -q "数据库迁移完成"
printf '%s' "$password" | "$platform_bin" admin init --database-url "$dsn" --username "$username" --password-stdin >/dev/null

source_root=$(mktemp -d)
"$platform_bin" serve --database-url "$dsn" --redis-addr localhost:6379 --public-addr "$public_addr" --transfer-addr "$transfer_addr" --public-base-url "$base_url" --session-key "$session_key" --csrf-key "$csrf_key" --master-key "$master_key" --oauth-key "$oauth_key" --source-root "$source_root" --allow-local-http &
server_pid=$!

for _ in $(seq 1 80); do
  curl -fsS "$base_url/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "$base_url/healthz" | grep -q '"status":"ok"' || { echo "healthz 未就绪" >&2; exit 1; }
echo "PASS 迁移与管理员初始化后控制面启动"

cookies=$(mktemp)
csrf=$(curl -fsS -c "$cookies" "$base_url/auth/csrf" | sed -n 's/.*"csrfToken":"\([^"]*\)".*/\1/p')
test -n "$csrf"
curl -fsS -b "$cookies" -c "$cookies" -X POST "$base_url/api/platform.v1.IdentityService/Login" -H "Content-Type: application/json" -H "X-CSRF-Token: $csrf" --data "{\"username\":\"$username\",\"password\":\"$password\"}" | grep -q '"instanceAdmin":true'
echo "PASS 真实登录并识别实例管理员"

csrf=$(curl -fsS -b "$cookies" "$base_url/auth/csrf" | sed -n 's/.*"csrfToken":"\([^"]*\)".*/\1/p')
curl -fsS -b "$cookies" -X POST "$base_url/api/platform.v1.WorkspaceService/CreateWorkspace" -H "Content-Type: application/json" -H "X-CSRF-Token: $csrf" --data '{"name":"冒烟空间","slug":"smoke-space"}' | grep -q '"slug":"smoke-space"'
echo "PASS 创建工作空间并持久化"

curl -fsS "$base_url/.well-known/oauth-authorization-server" | grep -q '"grant_types_supported"'
echo "PASS OAuth 元数据可用"

rm -f "$cookies"
echo "SMOKE PASS"
