#!/usr/bin/env bash
# 本地预览实例（127.0.0.1:8080）建立示例应用：<name> <port> <probe> <tar>
# 本地无执行节点：构建/部署止步于资格前置（如实展示），应用/配置/源码全量可用。
set -euo pipefail
BASE=http://127.0.0.1:8080
JAR=/tmp/local-session.jar
NAME=$1; PORT=$2; PROBE=$3; TAR=$4
_csrf() { curl -s -b "$JAR" -c "$JAR" "$BASE/auth/csrf" | python3 -c 'import json,sys;print(json.load(sys.stdin)["csrfToken"])'; }
rpc() {
  local rmethod=$1 rpayload=$2 csrf
  csrf=$(_csrf)
  local out
  out=$(curl -s -b "$JAR" -c "$JAR" -H "X-CSRF-Token: $csrf" -H "Content-Type: application/json" -d "$rpayload" "$BASE/api/platform.v1.$rmethod")
  if echo "$out" | grep -q '"code":"unauthenticated"'; then
    csrf=$(_csrf)
    curl -s -b "$JAR" -c "$JAR" -H "X-CSRF-Token: $csrf" -H "Content-Type: application/json" \
      -d '{"username":"e2eadmin","password":"e2e-admin-password"}' "$BASE/api/platform.v1.IdentityService/Login" >/dev/null
    csrf=$(_csrf)
    out=$(curl -s -b "$JAR" -c "$JAR" -H "X-CSRF-Token: $csrf" -H "Content-Type: application/json" -d "$rpayload" "$BASE/api/platform.v1.$rmethod")
  fi
  echo "$out"
}
WSID=$(rpc WorkspaceService/ListWorkspaces '{"limit":20}' | python3 -c '
import json,sys
for w in json.load(sys.stdin)["workspaces"]:
    if w["slug"]=="shop111222": print(w["id"]); break')
[ -z "$WSID" ] && WSID=$(rpc WorkspaceService/ListWorkspaces '{"limit":20}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["workspaces"][0]["id"])')
echo "workspace=$WSID"
APP=$(rpc ApplicationService/CreateApplication "{\"workspaceId\":\"$WSID\",\"name\":\"$NAME\",\"idempotencyKey\":\"ex-local-$NAME-1\"}" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["application"]["id"])' 2>/dev/null || true)
if [ -z "${APP:-}" ]; then
  APP=$(rpc ApplicationService/ListApplications "{\"workspaceId\":\"$WSID\",\"limit\":50}" | python3 -c '
import json,sys
for a in json.load(sys.stdin)["applications"]:
    if a["name"]=="'"$NAME"'": print(a["id"]); break')
fi
echo "app=$APP"
rpc RuntimeConfigService/SaveRuntimeConfig "{\"workspaceId\":\"$WSID\",\"applicationId\":\"$APP\",\"expectedRevision\":0,\"confirmed\":true,\"type\":\"HTTP\",\"port\":$PORT,\"probe\":{\"path\":\"$PROBE\",\"initialDelaySeconds\":0,\"periodSeconds\":10,\"timeoutSeconds\":2,\"failureThreshold\":3},\"resources\":{\"cpuRequestMilli\":100,\"cpuLimitMilli\":500,\"memoryRequestMib\":128,\"memoryLimitMib\":512},\"idempotencyKey\":\"ex-local-cfg-$NAME-1\"}" | python3 -c 'import json,sys;c=json.load(sys.stdin)["config"];print("配置 v",c["revision"],"confirmed:",c["confirmed"])'
SIZE=$(stat -f%z "$TAR" 2>/dev/null || stat -c%s "$TAR"); SHA=$(shasum -a 256 "$TAR" | cut -d' ' -f1)
UP=$(rpc SourceService/CreateSourceUpload "{\"workspaceId\":\"$WSID\",\"applicationId\":\"$APP\",\"size\":$SIZE,\"sha256\":\"$SHA\",\"dockerfile\":\"Dockerfile\"}")
UPID=$(echo "$UP" | python3 -c 'import json,sys;print(json.load(sys.stdin)["upload"]["id"])')
TPATH=$(echo "$UP" | python3 -c 'import json,sys;print(json.load(sys.stdin)["transferPath"])')
CSRF=$(_csrf)
PUT_CODE=$(curl -s -b "$JAR" -o /dev/null -w "%{http_code}" -X PUT --data-binary "@$TAR" "http://127.0.0.1:8081$TPATH" -H "Host: 127.0.0.1:8080" -H "X-CSRF-Token: $CSRF" -H "Content-Type: application/octet-stream")
echo "PUT=$PUT_CODE"
if [ "$PUT_CODE" != "204" ]; then
  PUT_CODE=$(curl -s -b "$JAR" -o /dev/null -w "%{http_code}" -X PUT --data-binary "@$TAR" "$BASE$TPATH" -H "X-CSRF-Token: $CSRF" -H "Content-Type: application/octet-stream")
  echo "PUT(8080)=$PUT_CODE"
fi
rpc SourceService/CompleteSourceUpload "{\"workspaceId\":\"$WSID\",\"applicationId\":\"$APP\",\"uploadId\":\"$UPID\"}" | python3 -c 'import json,sys;s=json.load(sys.stdin)["snapshot"];print("snapshot:",s["id"],s["state"])'
echo "== $NAME 完成（构建/部署需执行节点，本地实例止步于资格前置） =="
