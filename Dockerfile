# ---- build ----
# 依赖全是纯 Go（gin / mysql 驱动 / jwt），CGO_ENABLED=0 即可静态编译出单二进制
FROM golang:1.26-alpine AS build
WORKDIR /src
# 国内网络兜底：模块代理加速
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/imgbed ./cmd/imgbed

# ---- runner ----
FROM alpine:3.20
WORKDIR /data
COPY --from=build /out/imgbed /usr/local/bin/imgbed
RUN apk add --no-cache ca-certificates
EXPOSE 18080
ENTRYPOINT ["imgbed"]
CMD ["-config", "/data/config.yaml"]
