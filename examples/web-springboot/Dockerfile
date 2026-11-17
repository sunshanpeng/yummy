# 多阶段构建：Maven 编译 fat jar，运行镜像非 root。首次构建会下载依赖，
# 构建时间较长属正常现象；BuildKit 层缓存让后续构建明显加快。
FROM maven:3.9-eclipse-temurin-21 AS build
WORKDIR /src
COPY pom.xml ./
RUN mvn -q dependency:go-offline
COPY src ./src
RUN mvn -q package -DskipTests

FROM eclipse-temurin:21-jre-alpine
RUN addgroup -g 1000 app && adduser -u 1000 -G app -D app
COPY --from=build /src/target/web-springboot-1.0.0.jar /app.jar
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["java", "-jar", "/app.jar"]
