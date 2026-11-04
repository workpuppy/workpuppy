# syntax=docker/dockerfile:1
# 目标形态：**单个 Alpine 镜像，全部由 Go 构建，不含 python3**。
# 构建镜像只用来编译；运行镜像里只有静态链接的 Go 二进制 + bash（shell 运维脚本）。
FROM golang:1.26-alpine AS build
# go mod download 默认走 proxy.golang.org，国内网络不可达时构建失败。默认改走
# goproxy.cn（公共 CDN，海外同样可达，回源走 direct）。要换代理时用构建参数覆盖：
#   docker compose build --build-arg GOPROXY=https://proxy.golang.org,direct
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# 一次编译全部二进制（全部 -trimpath -s -w，CGO 关闭以便静态链接到 alpine）。
# 为什么这么多小工具：容器内要能直接跑运维命令（登录 / 签到 / 积分 / 任务 / 管理面），
# 而不是让用户再装一套运行时。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trial_bin ./cmd/trial \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/activity_bin ./cmd/activity \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/task_runner ./cmd/taskrunner \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/school_bin ./cmd/school \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wbctl ./cmd/wbctl

FROM alpine:3.20
# Alpine 官方 CDN 在国内经常超时。构建参数可覆盖：
#   docker compose build --build-arg APK_MIRROR=dl-cdn.alpinelinux.org
ARG APK_MIRROR=mirrors.aliyun.com
# 只装运行期必需物：wget（HEALTHCHECK）、ca-certificates（出站 TLS）、tzdata
# （本地时区计算：签到/随机排程/计费窗口都依赖它）、bash（运维 shell 脚本）。
# **不装 python3**：原有 python 脚本已全部 Go 化，镜像里不再有解释器依赖。
RUN if [ -n "$APK_MIRROR" ]; then sed -i "s#dl-cdn.alpinelinux.org#$APK_MIRROR#g" /etc/apk/repositories; fi \
 && apk add --no-cache wget ca-certificates tzdata bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 二进制与脚本在切到 app 之前以 root 完成（app 对 root 所有的文件无写权限，
# sed -i 去 CRLF 需要写权限）。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY --from=build /out/trial_bin /app/trial_bin
COPY --from=build /out/activity_bin /app/activity_bin
COPY --from=build /out/task_runner /app/task_runner
COPY --from=build /out/school_bin /app/school_bin
COPY --from=build /out/wbctl /app/wbctl
COPY login.sh signin.sh credit.sh trial.sh /app/
RUN sed -i 's/\r$//' /app/*.sh && chmod 755 /app/*.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
