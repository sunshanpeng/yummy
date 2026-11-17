# TanStack Start（Nitro SSR）构建输出 .output/server/index.mjs；运行镜像自带
# uid 1000 的 node 用户（非 root）。PORT 环境变量控制监听端口。
FROM node:22-alpine AS build
WORKDIR /app
COPY package.json ./
RUN npm install --no-audit --no-fund
COPY . .
RUN npm run build

FROM node:22-alpine
WORKDIR /app
COPY --from=build --chown=node:node /app/.output /app/.output
USER node
ENV PORT=3000 NODE_ENV=production
EXPOSE 3000
CMD ["node", ".output/server/index.mjs"]
