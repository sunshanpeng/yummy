#!/usr/bin/env bash
# 重置本机 e2e 实例数据：专用库整体重建 + 迁移 + 首位管理员。
# 仅用于临时 e2e 实例；绝不指向任何有真实数据的库。
# 用法：scripts/reset-e2e.sh [管理员用户名] [管理员密码]
set -eu

container=${YUMMY_E2E_PG_CONTAINER:-yummy-postgres-1}
database=${YUMMY_E2E_PG_DATABASE:-yummy_e2e}
dsn_user=${YUMMY_E2E_PG_USER:-demo}
username=${1:-e2eadmin}
password=${2:-e2e-admin-password}
binary=${E2E_PLATFORM_BIN:-bin/yummy-server}

test -x "$binary" || { echo "缺少 ${binary}，请先 make build-server" >&2; exit 1; }

docker exec "$container" psql -U "$dsn_user" -d postgres -c "DROP DATABASE IF EXISTS $database" >/dev/null
docker exec "$container" psql -U "$dsn_user" -d postgres -c "CREATE DATABASE $database OWNER $dsn_user" >/dev/null

"$binary" migrate --database-url "postgres://$dsn_user:$dsn_user@localhost:5432/$database?sslmode=disable"
printf '%s' "$password" | "$binary" admin init --database-url "postgres://$dsn_user:$dsn_user@localhost:5432/$database?sslmode=disable" --username "$username" --password-stdin >/dev/null
# 注意：$database 后紧跟全角逗号，必须用大括号定界变量名，否则 bash 会把多字节字符首字节并入变量名导致 unbound variable。
echo "e2e 实例数据已重置（库 ${database}，管理员 ${username}）"
