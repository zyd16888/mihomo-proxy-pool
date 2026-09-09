# Mihomo Manager

一个面向固定代理线路的 Web 管理工具。一个 Docker 镜像、一个容器，内置官方 Mihomo 内核与 Go 管理服务；不再提供原项目的用户名选路网关。

## 使用方式

1. 在页面添加订阅并同步节点，或粘贴节点配置进行手动导入。
2. 新增监听，填写本地端口并选择一个节点。
3. 保存后自动校验、重载并检查监听状态。
4. 爬虫和 Byparr 都使用 `http://宿主机IP:监听端口`。

支持新增、编辑、换绑、启停和删除监听。端口使用 `mixed`，同时提供 HTTP / SOCKS5；监听固定绑定具体节点，无随机分配或自动故障切换。删除监听不会删除节点，节点被引用时不能直接删除。

## Docker 部署

适用于 Linux Docker / 支持 host 网络的 Linux NAS。Windows/macOS Docker Desktop 的 host 网络能力依赖其版本与设置，不属于当前容器验收范围。

```sh
cp .env.example .env
# 编辑 .env，设置至少 12 字符的 ADMIN_KEY。
docker compose up -d --build
```

打开 `http://宿主机IP:3481`，使用 `ADMIN_KEY` 登录。首次启动自动创建数据库和最小运行配置，不需要预先提供订阅或配置文件。

`network_mode: host` 让 Mihomo 直接监听宿主机端口，因此页面新增端口后不必修改 `ports` 或重建容器。可使用 1–65535 中未被占用的端口，管理页与内核控制端口保留不可用于代理。低位端口需部署进程具备相应权限；默认镜像以 root 运行。

默认仅管理页监听 3481，内核控制接口绑定 `127.0.0.1:9090` 并使用自动生成的独立口令。不会开启原网关的 3480/3482，也不会因导入订阅就开放所有节点端口。

代理监听本身不要求认证，部署时仅向爬虫和过盾服务所在的可信网络开放。容器内服务访问宿主机时使用可达的宿主机地址，不要把容器自身的 `127.0.0.1` 当成代理宿主机。

### 配置

| 环境变量 | 默认值 | 作用 |
| --- | --- | --- |
| `ADMIN_KEY` | 必填 | 管理页登录口令，至少 12 字符 |
| `DATA_DIR` | 镜像内 `/data` | 持久数据目录 |
| `CONTROL_ADDR` | `0.0.0.0:3481` | 管理页地址 |
| `MIHOMO_CONTROL_ADDR` | `127.0.0.1:9090` | 必须绑定回环地址的内核控制接口 |
| `MIHOMO_BIN` | 镜像内 `/usr/local/bin/mihomo` | 内核二进制路径 |
| `TZ` | `Asia/Shanghai` | 时区 |

核心版本在 Docker 构建参数 `MIHOMO_VERSION` 中固定为 `v1.19.30`。默认构建复用官方镜像中的二进制，支持构建 `linux/amd64` 与 `linux/arm64`；平台跟随构建目标，不再硬编码 amd64。修改内核版本后重新构建镜像。

```sh
docker compose logs -f --tail=100
docker compose ps
docker compose down
```

健康检查同时检查管理 HTTP 服务和内核。Tini 与 Supervisor 负责信号和进程回收；管理服务先停止，内核后停止。进程异常退出会重启，启动重试耗尽时关闭整个容器，交给 Docker 的 `unless-stopped` 策略恢复。

## 数据与应用语义

`./data` 是唯一需要备份的目录：

- `manager.db`：订阅、节点、监听、保存版本和应用状态，SQLite WAL 模式。
- `config.yaml`：内核运行配置，由管理服务维护。
- `last-good.yaml`：上一次应用并验证成功的配置。
- `core.secret`：管理服务调用内核的独立认证口令。

完整备份应先停止容器再复制整个目录，避免遗漏 SQLite WAL。不要手动编辑生成的 YAML，也不要让多个管理实例共享同一个数据目录。

每次变更先保存配置，再串行执行以下流程：生成候选配置 → 检查新增端口冲突 → `mihomo -t` 校验 → 写入并重载 → 检查启用端口的 SOCKS 握手及旧端口关闭 → 更新已应用版本。

校验失败时不会替换运行文件。重载或监听验证失败时恢复上一版运行配置；数据库中的修改保留为“待应用”，可编辑修正后重试。页面显示具体错误，不将“已保存”视为“已生效”。启动时先加载上一版成功配置，管理服务待内核就绪后再尝试应用当前保存版本。

“已生效”表示配置已应用且监听已启动，不表示远端节点可连接或目标网站可访问；节点的“检测”使用 Mihomo 延迟检测 API，不依赖自建转发网关。端口换绑影响新连接，已有长连接可能持续到其自然关闭；若需要完全切断旧会话，应先停用并让客户端关闭连接，再重新启用。

## 订阅与节点

支持 Clash YAML、完整 JSON 节点数组、SS / Trojan / VLESS / VMess URI，以及上述内容的 Base64 编码。复杂协议参数优先使用完整 Clash YAML/JSON；URI 转换继承原项目的协议覆盖范围，不能假设能表达所有 Mihomo 扩展选项。

- 节点远程端口与本地监听端口完全独立。
- 同一来源的节点名称必须唯一，重复名称或部分格式错误会拒绝整批导入。
- 同来源同名节点更新时保留 ID，因此端口绑定不会因重复同步而改变。
- 节点重命名时，若能按完整代理配置唯一匹配原节点，则保留 ID；有歧义时不会猜测。
- 订阅中消失的节点保留为“不可用”，绑定引用保留，但关联监听会关闭；不会自动换绑其他节点。
- 请求失败、空订阅或解析失败不清空原节点。停用状态在重复同步后保持。
- 订阅同步由页面手动触发；当前没有定时订阅更新任务。

手动节点修改可通过导入相同名称的完整配置完成。

## 开发和验证

需要 Go 1.25+；Web 页面使用原生 HTML/CSS/JavaScript 并通过 `go:embed` 嵌入二进制，无需安装 Node 或构建前端。

```sh
go test ./...
go vet ./...
go build -o bin/mihomo-manager ./cmd/mihomo-manager
```

真实内核集成测试使用本地 HTTP 代理夹具，不访问真实订阅或外部站点：

```sh
MIHOMO_TEST_BIN=/absolute/path/to/mihomo go test -v ./internal/manager -run TestRealKernelListenerLifecycle
```

Windows PowerShell：

```powershell
$env:MIHOMO_TEST_BIN = 'D:/tools/mihomo.exe'
go test -v ./...
```

本地分进程运行：设置 `ADMIN_KEY`、`DATA_DIR`、`CONTROL_ADDR`、`MIHOMO_CONTROL_ADDR`、`MIHOMO_BIN`，先执行 `mihomo-manager -bootstrap`，再使用相同数据目录启动 `mihomo -d <DATA_DIR> -f <DATA_DIR>/config.yaml`，最后启动管理服务。该方式用于开发，Docker 镜像已自动完成这些步骤。

具体接口见 [API 文档](docs/API.md)，本次验证记录见 [验证记录](docs/VALIDATION.md)。

## 来源与项目边界

本项目基于 [rich/public/mihomo-proxy](https://cnb.cool/rich/public/mihomo-proxy) 改造，保留并调整了节点配置解析逻辑。管理服务、存储和监听页面采用新模型，不兼容原项目数据库和代理网关 API；旧 `generated/proxy.db` 不会被自动迁移或覆盖。

移除了原网关 HTTP/SOCKS5 转发、用户名选路、网关分组策略和网关会话日志。官方 Mihomo 内核作为独立进程随镜像打包，不嵌入其 Go 源码。

原下载版本未附上游许可证，公开可读并不自动等于 MIT；本次没有为上游代码另行声明 MIT。第三方来源见 [第三方说明](docs/THIRD_PARTY_NOTICES.md)。
