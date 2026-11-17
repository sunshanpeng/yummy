# 多阶段构建：编译产出静态二进制，运行镜像非 root（平台要求 runAsNonRoot）。
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server .

FROM alpine:3.20
RUN addgroup -g 1000 app && adduser -u 1000 -G app -D app
COPY --from=build /out/server /server
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/server"]
