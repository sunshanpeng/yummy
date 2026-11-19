PROTOC ?= $(shell command -v protoc 2>/dev/null || echo work/protoc/bin/protoc)

# 国内网络可通过 goproxy.cn 加速 Go 模块下载；也可自行覆盖，例如：
#   GOPROXY=https://proxy.golang.org,direct make build
export GOPROXY ?= https://goproxy.cn,https://proxy.golang.org,direct
export GOSUMDB ?= sum.golang.google.cn

.PHONY: install generate web-build build test integration-test smoke

install:
	pnpm --dir web install --frozen-lockfile

generate:
	test -x "$(PROTOC)"
	mkdir -p work/tools gen/go web/src/gen
	go build -o work/tools/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	go build -o work/tools/protoc-gen-connect-go connectrpc.com/connect/cmd/protoc-gen-connect-go
	PATH="$(CURDIR)/work/tools:$(CURDIR)/web/node_modules/.bin:$$PATH" "$(PROTOC)" \
		-I api \
		--go_out=gen/go \
		--go_opt=paths=source_relative \
		--connect-go_out=gen/go \
		--connect-go_opt=paths=source_relative \
		--es_out=web/src/gen \
		--es_opt=target=ts \
		api/platform/v1/*.proto

web-build:
	pnpm --dir web build
	rsync -a --delete web/dist/ internal/controlplane/webui/dist/

build: web-build
	mkdir -p bin
	go build -trimpath -ldflags="-s -w" -o bin/platform ./cmd/platform

test:
	go test ./... -race -count=1
	pnpm --dir web test --run

# 真实存储集成测试：需要 compose 起本机 PostgreSQL 与 Redis（见 compose.yaml）。
integration-test:
	TEST_DATABASE_URL="postgres://demo:demo@localhost:5432/demo?sslmode=disable" \
	TEST_REDIS_ADDR="localhost:6379" \
	go test ./internal/... -count=1

# 产品冒烟：对 bin/platform 做迁移/管理员/登录/建空间/OAuth 元数据最小真实链路验证。
smoke: build
	./scripts/smoke-product.sh
