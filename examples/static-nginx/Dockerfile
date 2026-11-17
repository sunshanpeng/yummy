# nginx-unprivileged 自带非 root 用户（uid 101）并默认监听 8080，
# 与"静态站点 / 现成镜像"模板对应；官方 nginx 需要 root，平台不接受。
FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY index.html /usr/share/nginx/html/index.html
EXPOSE 8080
