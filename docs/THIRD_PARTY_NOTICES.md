# 第三方来源

- 原始项目：https://cnb.cool/rich/public/mihomo-proxy ，本次下载版本对应调研提交 `6886792f96ebb3472f7d0f9ecb30ad0e9871d042` 的结构。保留并调整了 `internal/service/node_import_proxy.go` 等节点解析逻辑，现位于 `internal/importer/parser.go`。原下载目录没有许可证文件，本项目未替其声明 MIT。
- Mihomo 内核：官方 `metacubex/mihomo:v1.19.30`，对应源码 https://github.com/MetaCubeX/mihomo/tree/v1.19.30 。其许可证为 GPL-3.0，原文见 `third-party/mihomo-LICENSE`。内核作为单独二进制运行。
- Go 第三方依赖及版本记录在 `go.mod` / `go.sum` 中；容器运行时由 Alpine 包管理器安装 Tini、Supervisor 等组件。

该文件用于记录来源，不授予原作者未提供的许可，也不改变各第三方组件的许可证。
