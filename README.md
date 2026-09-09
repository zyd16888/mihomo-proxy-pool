# Mihomo Manager

一个面向固定代理线路的 Web 管理工具。一个 Docker 镜像、一个容器，内置官方 Mihomo 内核与 Go 管理服务；不再提供原项目的用户名选路网关。

## 使用方式

1. 在页面添加订阅并同步节点，或粘贴节点配置进行手动导入。
2. 新增监听，填写本地端口并选择一个节点。
3. 保存后自动校验、重载并检查监听状态。
4. 爬虫和 Byparr 都使用 `http://宿主机IP:监听端口`。

支持新增、编辑、换绑、启停和删除监听。端口使用 `mixed`，同时提供 HTTP / SOCKS5；监听固定绑定具体节点，无随机分配或自动故障切换。删除监听不会删除节点，节点被引用时不能直接删除。

## 使用 GHCR 镜像部署

适用于 Linux Docker / 支持 host 网络的 Linux NAS。Windows/macOS Docker Desktop 的 host 网络能力依赖其版本与设置，不属于当前容器验收范围。

普通用户只需要 `docker-compose.yml` 和 `.env.example`，不需要下载源码、安装 Go 或构建镜像。

```sh
cp .env.example .env
# 编辑 .env：设置 ADMIN_KEY，并将 MIHOMO_IMAGE 改为项目的实际镜像地址。
docker compose pull
docker compose up -d
```

本项目镜像地址为 `ghcr.io/zyd16888/mihomo-proxy-pool:latest`。也可以使用固定版本，例如 `:1.2.3`。实际发布的地址会显示在 GitHub Actions 的运行摘要和仓库 Packages 中；首次成功发布前，该镜像尚不可拉取。

Mihomo 内核已经包含在镜像内，路径由镜像维护，用户无需配置或挂载内核二进制。

打开 `http://宿主机IP:3481`，使用 `ADMIN_KEY` 登录。首次启动自动创建数据库和最小运行配置，不需要预先提供订阅或配置文件。

`network_mode: host` 让 Mihomo 直接监听宿主机端口，因此页面新增端口后不必修改 `ports` 或重建容器。可使用 1–65535 中未被占用的端口，管理页与内核控制端口保留不可用于代理。低位端口需部署进程具备相应权限；默认镜像以 root 运行。

默认仅管理页监听 3481，内核控制接口绑定 `127.0.0.1:9090` 并使用自动生成的独立口令。不会开启原网关的 3480/3482，也不会因导入订阅就开放所有节点端口。

代理监听本身不要求认证，部署时仅向爬虫和过盾服务所在的可信网络开放。容器内服务访问宿主机时使用可达的宿主机地址，不要把容器自身的 `127.0.0.1` 当成代理宿主机。

### 配置

| 环境变量 | 默认值 | 作用 |
| --- | --- | --- |
| `ADMIN_KEY` | 必填 | 管理页登录口令，至少 12 字符 |
| `MIHOMO_IMAGE` | 必填 | Compose 使用的已发布镜像地址；不传入应用容器 |
| `DATA_DIR` | 镜像内 `/data` | 持久数据目录 |
| `CONTROL_ADDR` | `0.0.0.0:3481` | 管理页地址 |
| `MIHOMO_CONTROL_ADDR` | `127.0.0.1:9090` | 必须绑定回环地址的内核控制接口 |
| `TZ` | `Asia/Shanghai` | 时区 |

已发布镜像同时包含 `linux/amd64` 和 `linux/arm64`，Docker 会自动选择对应架构。内核版本由项目维护者在镜像构建时确定，用户通过更新镜像升级。

```sh
# 更新镜像并重建容器，./data 中的数据保留
docker compose pull
docker compose up -d

docker compose logs -f --tail=100
docker compose ps
docker compose down
```

健康检查同时检查管理 HTTP 服务和内核。Tini 与 Supervisor 负责信号和进程回收；管理服务先停止，内核后停止。进程异常退出会重启，启动重试耗尽时关闭整个容器，交给 Docker 的 `unless-stopped` 策略恢复。

## GitHub Actions 发布镜像

工作流：[`.github/workflows/publish-image.yml`](.github/workflows/publish-image.yml)。

- 推送到 `main` / `master`、推送 `v*` 标签，或在 Actions 中手动执行时触发。
- 先构建 amd64 测试镜像，用镜像内核运行现有测试（包括真实监听联调和 race）、`go vet`，再检查容器空目录启动和整容器重启。
- 检查成功后构建并发布 `linux/amd64,linux/arm64` 多架构镜像到 `ghcr.io/<owner>/<repo>`。
- 镜像名自动使用当前 GitHub 仓库并转为小写，无需在工作流中填写仓库地址。
- 默认分支发布 `latest`，分支还会发布 `main` / `master` 标签；语义版本标签 `v1.2.3` 发布 `1.2.3`、`1.2`，稳定版本也更新 `latest`。预发布版本不自动更新 `latest`。每次构建另带 `sha-<完整提交号>`。

推送代码到自己的 GitHub 仓库后，确认 Actions 已启用。工作流使用自带的 `GITHUB_TOKEN`，只在发布任务授予 `packages: write`，通常不需要额外配置 PAT。

**首次发布后，需要到 GHCR 包的 Package settings 将可见性设为 Public，普通用户才能免登录拉取。** GitHub 仓库公开不代表新建的 GHCR 包自动公开。如果同名包已经存在，还需确认它允许当前仓库的 Actions 写入。

工作流文件的加入不会立即发布镜像；只有提交并推送到 GitHub 后，才会在 Actions 中执行。当前 GitHub 仓库为 [zyd16888/mihomo-proxy-pool](https://github.com/zyd16888/mihomo-proxy-pool)；Fork 后工作流会自动发布到 Fork 仓库对应的 GHCR 地址，部署时同步修改 `.env` 中的镜像地址。

## 数据与应用语义

`./data` 是唯一需要备份的目录：

- `manager.db`：订阅、用量快照、节点、监听、保存版本和应用状态，SQLite WAL 模式。
- `config.yaml`：内核运行配置，由管理服务维护。
- `last-good.yaml`：上一次应用并验证成功的配置。
- `core.secret`：管理服务调用内核的独立认证口令。

完整备份应先停止容器再复制整个目录，避免遗漏 SQLite WAL。不要手动编辑生成的 YAML，也不要让多个管理实例共享同一个数据目录。

每次变更先保存配置，再串行执行以下流程：生成候选配置 → 检查新增端口冲突 → `mihomo -t` 校验 → 写入并重载 → 检查启用端口的 SOCKS 握手及旧端口关闭 → 更新已应用版本。

校验失败时不会替换运行文件。重载或监听验证失败时恢复上一版运行配置；数据库中的修改保留为“待应用”，可编辑修正后重试。页面显示具体错误，不将“已保存”视为“已生效”。启动时先加载上一版成功配置，管理服务待内核就绪后再尝试应用当前保存版本。

“已生效”表示配置已应用且监听已启动，不表示远端节点可连接或目标网站可访问；节点的“检测”使用 Mihomo 延迟检测 API，不依赖自建转发网关。端口换绑影响新连接，已有长连接可能持续到其自然关闭；若需要完全切断旧会话，应先停用并让客户端关闭连接，再重新启用。

## 订阅与节点

支持 Clash YAML、完整 JSON 节点数组、SS / Trojan / VLESS / VMess URI，以及上述内容的 Base64 编码。VLESS URI 支持 TCP、WebSocket、gRPC，以及 TLS / Reality、Vision flow、SNI、客户端指纹、ALPN 和 UDP 包编码。暂不支持的关键 URI 参数会明确报错，复杂配置仍应使用完整 Clash YAML/JSON。

- 节点远程端口与本地监听端口完全独立。
- 同一来源的节点名称必须唯一，重复名称或部分格式错误会拒绝整批导入。
- 同来源同名节点更新时保留 ID，因此端口绑定不会因重复同步而改变。
- 节点重命名时，若能按完整代理配置唯一匹配原节点，则保留 ID；有歧义时不会猜测。
- 订阅中消失的节点保留为“不可用”，绑定引用保留，但关联监听会关闭；不会自动换绑其他节点。
- 请求失败、空订阅或解析失败不清空原节点。停用状态在重复同步后保持。
- 订阅同步由页面手动触发；当前没有定时订阅更新任务。

手动节点修改可通过导入相同名称的完整配置完成。

## 订阅用量和节点卡片

订阅页及节点分组标题显示账户已用、总量、剩余、超额、到期时间和更新时间。来源是订阅 HTTP 响应头 `subscription-userinfo`：

```http
subscription-userinfo: upload=1073741824; download=2147483648; total=10737418240; expire=2000000000
```

上传和下载以字节计数，已用为两者之和；剩余最小为 0，超额另行展示。界面按 IEC 单位换算（1 GiB = 1024³ 字节）。缺失字段显示“未提供”；总量为 0 时不推断成无限流量，到期为 0 时不推断成永久有效。

- “刷新用量”只请求订阅响应并更新账户快照，不导入节点、不增加配置版本、不重载 Mihomo。
- “同步节点”也会读取该响应头。合法用量与节点解析独立，节点正文解析失败时可更新合法用量，原节点保持不变。
- 没有响应头、格式异常或网络失败时保留最近有效快照并标注刷新状态，不使用旧字段拼接新快照。
- 同一订阅同时只允许一次刷新；订阅 URL 改变会清除旧账户快照，旧 URL 的迟到响应不能写入。
- 用量保存在新增的 `subscription_usage` 表中；旧数据库启动时自动补建，既有节点和监听不需要重建。

这是机场返回的账户用量，可能包括其他客户端的使用，不是本程序实时统计。数据更新于刷新订阅，页面自动刷新只读取当前快照。

节点卡片省略远程地址和重复来源信息。点击节点名称或“更多 → 详情”查看远程地址、绑定监听和检测错误；更多菜单提供启停和删除。分组可以折叠，并支持对当前筛选结果进行组内检测；监听绑定仍在监听管理中操作。

### 修复已导入的 VLESS 节点

旧 URI 转换器没有保存 Reality / Vision 的完整参数。升级程序后，在“导入节点”中以原名称重新导入原始完整链接，补回 `tls`、`flow`、`reality-opts`、`client-fingerprint` 等字段；同名手动节点会保留 ID 和监听引用。已经丢失的字段无法仅凭旧数据库中的 UUID 和服务器地址还原。

不需要先删除旧节点。若来自 URI 订阅，可重新同步对应订阅。实际连通性仍取决于完整链接参数、服务端与当前内核的兼容性及网络条件。

## 节点延迟与批量检测

节点池按订阅分组展示紧凑卡片，手动节点单独一组。卡片右侧显示最近一次检测结果，完成时间和失败原因可在节点详情中查看，支持未检测、排队中、检测中、成功、超时、失败、取消和过期状态。测试目标沿用 `https://www.gstatic.com/generate_204`，内核请求超时 5 秒；该值代表访问测试地址的耗时，不代表采集站点速度。

“批量检测 (N)”只检测当前搜索结果中的已启用、有效节点。后台最多同时执行 4 个检测，单节点检测与批量共用并发限制；同一节点的在途请求会复用。进度逐项更新，检测期间可继续搜索和浏览。点击“停止检测”会取消该批次排队和执行中的任务；批次复用的、此前独立发起的单节点检测可以继续完成。

检测结果和批量任务仅保存在管理进程内存中：刷新或重新打开页面可以继续查看，管理服务重启后清空。检测不写入 SQLite，不增加配置版本，也不触发内核重载。节点配置变更后旧结果标记过期，重新检测才会产生新延迟。

当前配置尚未成功应用、内核未就绪，或节点已停用/移除时不能开始检测。批量任务启动后锁定目标范围，后续改变搜索条件只影响列表展示。

分流规则仍作为独立功能，本次检测功能不会改变固定端口与节点的路由关系。

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

本地分进程运行：将官方内核命名为 `mihomo`（Windows 为 `mihomo.exe`）并加入 `PATH`；设置 `ADMIN_KEY`、`DATA_DIR`、`CONTROL_ADDR`、`MIHOMO_CONTROL_ADDR`，先执行 `mihomo-manager -bootstrap`，再使用相同数据目录启动 `mihomo -d <DATA_DIR> -f <DATA_DIR>/config.yaml`，最后启动管理服务。该方式用于开发，Docker 镜像已自动完成这些步骤。

开发者需要本地构建容器时使用单独的覆盖文件，普通部署不使用它：

```sh
MIHOMO_IMAGE=mihomo-manager:local docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
```

内核构建版本在 `Dockerfile` 的 `MIHOMO_VERSION` 中固定为 `v1.19.30`；本地覆盖文件使用同一版本。维护者调整版本后，应通过工作流重新测试并发布镜像。

具体接口见 [API 文档](docs/API.md)，本次验证记录见 [验证记录](docs/VALIDATION.md)。

## 来源与项目边界

本项目基于 [rich/public/mihomo-proxy](https://cnb.cool/rich/public/mihomo-proxy) 改造，保留并调整了节点配置解析逻辑。管理服务、存储和监听页面采用新模型，不兼容原项目数据库和代理网关 API；旧 `generated/proxy.db` 不会被自动迁移或覆盖。

移除了原网关 HTTP/SOCKS5 转发、用户名选路、网关分组策略和网关会话日志。官方 Mihomo 内核作为独立进程随镜像打包，不嵌入其 Go 源码。

原下载版本未附上游许可证，公开可读并不自动等于 MIT；本次没有为上游代码另行声明 MIT。第三方来源见 [第三方说明](docs/THIRD_PARTY_NOTICES.md)。
