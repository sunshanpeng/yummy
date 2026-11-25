# 由安装/发布流水线显式传入固定镜像，不提供latest或自动获取digest的默认值。
# 示例参数：GO_BUILDER=<go版本镜像>@sha256:<摘要>
# BUILDKIT_BASE=moby/buildkit:<固定版本>-rootless@sha256:<摘要>
# Railpack 固定到已核对 Mise 安全模式的官方 v0.43.0，worker 须配置该版本的前端镜像 digest。
# 产出的自有runner镜像也必须在worker配置中以@sha256固定，真实AMD64资格验证后才能启用。
ARG GO_BUILDER
ARG BUILDKIT_BASE
FROM --platform=$BUILDPLATFORM ${GO_BUILDER} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/infra/buildrunner ./internal/infra/buildrunner
COPY internal/infra/sourcearchive ./internal/infra/sourcearchive
COPY internal/lib/observability ./internal/lib/observability
COPY cmd/build-runner ./cmd/build-runner
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/yummy-runner ./cmd/build-runner
# Railpack 只用于没有 Dockerfile 的源码，按官方发行包摘要校验。
FROM --platform=$BUILDPLATFORM ${GO_BUILDER} AS railpack
RUN wget -q -O /tmp/railpack.tar.gz "https://github.com/railwayapp/railpack/releases/download/v0.43.0/railpack-v0.43.0-x86_64-unknown-linux-musl.tar.gz" \
 && echo "248a8f60f36854aabed6124113631078a802de3461660855fc4de9c26065c1fb  /tmp/railpack.tar.gz" | sha256sum -c - \
 && mkdir /out && tar -xzf /tmp/railpack.tar.gz -C /out && test -x /out/railpack
FROM ${BUILDKIT_BASE}
USER root
COPY --from=build /out/yummy-runner /usr/local/bin/yummy-runner
COPY --from=railpack /out/railpack /usr/local/bin/railpack
# 私有CA应由安装流水线放入系统信任库；不要在容器内关闭TLS验证。
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/yummy-runner"]
