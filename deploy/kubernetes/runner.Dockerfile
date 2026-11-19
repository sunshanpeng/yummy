# 由安装/发布流水线显式传入固定镜像，不提供latest或自动获取digest的默认值。
# 示例参数：GO_BUILDER=<go版本镜像>@sha256:<摘要>
# BUILDKIT_BASE=moby/buildkit:<固定版本>-rootless@sha256:<摘要>
# 产出的自有runner镜像也必须在worker配置中以@sha256固定，真实AMD64资格验证后才能启用。
ARG GO_BUILDER
ARG BUILDKIT_BASE
FROM --platform=$BUILDPLATFORM ${GO_BUILDER} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/infra/buildrunner ./internal/infra/buildrunner
COPY internal/infra/sourcearchive ./internal/infra/sourcearchive
COPY cmd/build-runner ./cmd/build-runner
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/yummy-runner ./cmd/build-runner
FROM ${BUILDKIT_BASE}
USER root
COPY --from=build /out/yummy-runner /usr/local/bin/yummy-runner
# 私有CA应由安装流水线放入系统信任库；不要在容器内关闭TLS验证。
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/yummy-runner"]
