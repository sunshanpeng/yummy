# 部署示例

每个目录都是可以在 Yummy 平台直接构建、部署的最小示例：语言各不相同、
运行镜像一律**非 root**（平台安全基线要求 runAsNonRoot）、监听端口与健康
探针路径与控制台"运行配置"模板对齐。

| 目录 | 语言 / 形态 | 监听端口 | 探针路径 | 对应运行模板 |
|---|---|---|---|---|
| [go-http](go-http/) | Go net/http | 8080 | `/health` | Go HTTP 服务 |
| [web-nextjs](web-nextjs/) | Next.js 15（standalone） | 3000 | `/healthz` | Node.js 服务 |
| [web-tanstack-start](web-tanstack-start/) | TanStack Start 1.130（SSR） | 3000 | `/healthz`（返回 200 页面） | Node.js 服务 |
| [web-python-fastapi](web-python-fastapi/) | Python FastAPI | 8080 | `/health` | 手动配置（端口 8080） |
| [web-springboot](web-springboot/) | Java Spring Boot 3 | 8080 | `/health` | 手动配置（端口 8080） |
| [static-nginx](static-nginx/) | 静态站点（nginx-unprivileged） | 8080 | `/` | 静态站点 / 现成镜像 |

## 部署一个示例

1. **创建应用**：工作空间内只填应用名（创建后不可修改）。
2. **选择模板**：进入"部署 → 运行配置"，首次进入可选模板（或按上表手动填端口与探针路径），检查后**保存并确认运行配置**。
3. **上传源码**：在示例目录内打包后到"构建 → 源码"上传：

   ```sh
   tar -czf source.tar.gz --exclude '.git' --exclude 'node_modules' --exclude 'target' .
   ```

4. **构建**：到"构建"页预览输入后创建构建（构建参数/目标阶段已在 Dockerfile 内，无需额外设置）。
5. **部署**：构建成功后到"部署"页，用构建产出的镜像版本执行部署；部署完成后在部署页查看运行日志、基础监控与访问地址。

## 约定与注意

- **端口**：应用必须监听配置里填写的端口（默认 8080；Node 示例为 3000），且监听 `0.0.0.0`。
- **非 root**：运行镜像必须能以非 root 用户运行（平台以 runAsNonRoot 校验）。官方 nginx、需要写根目录或监听 80/443 的镜像会启动失败——示例均已处理。
- **健康探针**：探针路径返回 2xx 即健康（响应体不参与判定）；没有探针的 HTTP 应用会被视为不健康，建议都提供。
- **已验证**：全部 6 个示例均已本地 `docker build` + 运行验证（探针/首页 2xx）；TanStack Start 的 server routes 特性在其 1.130 版本尚未完全启用，`/healthz` 以页面路由响应 200，探针判定不受影响。
- **首次构建耗时**：Spring Boot（Maven 拉依赖）与 Node（npm install）首次构建较慢，BuildKit 层缓存会让后续构建明显加快。
