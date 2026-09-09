ARG MIHOMO_VERSION=v1.19.30
FROM metacubex/mihomo:${MIHOMO_VERSION} AS kernel
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /out/mihomo-manager ./cmd/mihomo-manager
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata tini supervisor
COPY --from=kernel /mihomo /usr/local/bin/mihomo
COPY --from=builder /out/mihomo-manager /usr/local/bin/mihomo-manager
COPY docker/ /etc/mihomo-manager/
COPY docs/THIRD_PARTY_NOTICES.md /usr/share/doc/mihomo-manager/THIRD_PARTY_NOTICES.md
COPY docs/third-party/ /usr/share/doc/mihomo-manager/third-party/
RUN chmod +x /etc/mihomo-manager/entrypoint.sh
ENV DATA_DIR=/data CONTROL_ADDR=0.0.0.0:3481 MIHOMO_CONTROL_ADDR=127.0.0.1:9090 TZ=Asia/Shanghai
VOLUME /data
EXPOSE 3481
HEALTHCHECK --interval=20s --timeout=5s --start-period=30s --retries=3 CMD ["/usr/local/bin/mihomo-manager", "-healthcheck"]
ENTRYPOINT ["/etc/mihomo-manager/entrypoint.sh"]
