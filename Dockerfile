# Build go
FROM golang:1.26.1-alpine AS builder
WORKDIR /app
COPY . .
ENV CGO_ENABLED=0
RUN GOEXPERIMENT=jsonv2 go mod download
RUN GOEXPERIMENT=jsonv2 go build -v -o sukad

# Release
FROM  alpine
# 安装必要的工具包
RUN  apk --update --no-cache add tzdata ca-certificates \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN mkdir /etc/sukad/
COPY --from=builder /app/sukad /usr/local/bin

ENTRYPOINT [ "sukad", "server", "--config", "/etc/sukad/config.json"]
