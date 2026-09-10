# 管理 API

除 `/healthz`、`/api/auth`、`/api/login` 外，所有接口需要登录 Cookie。写入请求使用 `application/json`，浏览器必须同源；不接受原代理网关 API。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/healthz` | 管理服务及内核健康；未就绪返回 503 |
| GET | `/api/auth` | 当前会话登录状态 |
| POST | `/api/login` | `{ "key": "管理口令" }` |
| POST | `/api/logout` | 注销并撤销当前 Cookie |
| GET | `/api/state` | 节点、监听、订阅、应用版本和内核状态 |
| POST | `/api/apply` | 重试应用当前保存配置 |
| POST | `/api/listeners` | 新增监听 |
| PUT | `/api/listeners/{id}` | 完整更新监听 |
| DELETE | `/api/listeners/{id}` | 删除监听，保留节点 |
| POST | `/api/import` | `{ "raw": "节点配置" }` 或 `{ "nodes": [...] }` |
| PATCH | `/api/nodes/{id}` | `{ "enabled": true }` |
| DELETE | `/api/nodes/{id}` | 删除未被引用的节点 |
| POST | `/api/nodes/{id}/check` | 异步单节点检测，返回 202 和检测快照 |
| GET | `/api/node-checks` | 读取当前内存结果和批量进度 |
| POST | `/api/node-checks/batch` | `{ "ids": ["节点 ID", "..."] }`，返回 202 和检测快照 |
| POST | `/api/node-checks/batch/{id}/stop` | 停止指定的当前批量任务 |
| POST | `/api/subscriptions` | 新增 `{ "name": "来源", "url": "https://..." }` |
| PUT | `/api/subscriptions/{id}` | 更新订阅名称与地址 |
| POST | `/api/subscriptions/{id}/sync` | 下载订阅并读取用量、同步节点 |
| POST | `/api/subscriptions/{id}/usage` | 只刷新订阅用量，不重载配置 |
| DELETE | `/api/subscriptions/{id}` | 删除订阅及其无引用节点 |
| GET | `/api/routing` | 分流设置、规则集、可选策略和订阅合并报告 |
| PUT | `/api/routing` | 保存分流设置 |
| POST | `/api/rule-sets` | 新增规则集 |
| PUT | `/api/rule-sets/{id}` | 完整更新规则集 |
| DELETE | `/api/rule-sets/{id}` | 删除规则集 |
| POST | `/api/rule-sets/refresh` | 立即让内核拉取全部启用的规则集 |
| GET | `/api/logs?since=` | 读取内核日志窗口中序号大于 `since` 的条目 |
| POST | `/api/logs/level` | `{ "level": "debug" }`，切换采集级别并重连日志流 |
| DELETE | `/api/logs` | 清空内存中的日志窗口 |
| GET | `/api/connections` | 当前连接，已还原为监听和节点名称 |
| DELETE | `/api/connections` | 关闭全部连接 |
| DELETE | `/api/connections/{id}` | 关闭单个连接 |
| GET | `/api/exit-ip` | 读取出口 IP 探测结果和进度 |
| POST | `/api/exit-ip/nodes` | `{ "ids": ["节点 ID"] }`，串行探测节点出口，返回 202 |
| POST | `/api/exit-ip/listeners/{id}` | 探测某个监听端口的出口，返回 202 |
| POST | `/api/exit-ip/batch/{id}/stop` | 停止指定的当前探测任务 |

监听写入示例。`mode` 为 `node` 时必须给出 `nodeId`；为 `rule` 时忽略 `nodeId`，出口由规则决定。省略 `mode` 按 `node` 处理，因此旧客户端行为不变。

```json
{"name":"采集线路 A","port":17891,"mode":"node","nodeId":"节点 ID","enabled":true}
{"name":"规则出口","port":17892,"mode":"rule","enabled":true}
```

变更成功保存时返回 HTTP 200：

```json
{"saved":true,"apply":{"applied":true,"revision":3}}
```

已保存但未应用时仍返回 HTTP 200，客户端必须检查 `apply.applied`：

```json
{"saved":true,"apply":{"applied":false,"revision":4,"error":"端口被占用"}}
```

数据校验或引用冲突返回 HTTP 400，不增加保存版本。认证失败返回 401，跨站写入返回 403。`/api/apply` 直接返回 `ApplyResult`，没有外层 `saved` / `apply`。

`GET /api/state` 不包含节点密码等完整代理配置；订阅 URL 仅向已认证管理员返回。代理 HTTP/SOCKS5 不走这些接口，而是直接连接实际监听端口。

## 检测快照

检测接口不返回配置变更的 `saved/apply` 包装，也不增加 `revision`。单节点接口现在异步返回 202，不再同步返回 `{ "delay": ... }`。开始检测后每秒读取 `/api/node-checks`；`/api/state` 的 `checks` 字段也包含同一快照。

```json
{
  "results": {
    "node-id": {"status":"success","delayMs":427,"checkedAt":"2026-09-09T04:00:00Z","inFlight":false}
  },
  "batch": {"id":"batch-id","status":"running","total":10,"completed":3,"succeeded":2,"failed":1,"skipped":0,"cancelled":0}
}
```

无记录表示未检测；失败结果没有 `delayMs`，可包含 `error`。结果状态为 `queued/running/success/timeout/failed/cancelled/stale/skipped`。批量状态为 `running/stopping/completed/stopped`；`completed` 统计已处理目标，包含成功、失败、跳过及取消。

`ids` 必须为非空数组，重复 ID 自动合并，无效或停用节点不进入批量目标。已有批量任务运行时复用当前任务，不额外启动另一个批次；停止接口需传回当前 `batch.id`，避免旧页面停止了新任务。检测的身份认证、同源限制与其他管理接口一致。

所有结果和任务均为进程内存状态，管理服务重启清空。


## 订阅用量

`GET /api/state` 的每个订阅可带 `usage` 对象。尚未获取用量时没有此对象；首次刷新未提供头时包含状态但没有数值。

```json
{"uploadBytes":100,"downloadBytes":300,"totalBytes":1000,"expire":2000000000,"usedBytes":400,"remainingBytes":600,"overageBytes":0,"status":"current","updatedAt":"2026-09-09T05:00:00Z","checkedAt":"2026-09-09T05:00:00Z"}
```

数值为字节，到期为 Unix 秒。缺失值为 `null`；用量任一计数缺失时 `usedBytes` 为 `null`，总量缺失/为 0 时 `remainingBytes` 为 `null`。状态为 `current/missing/invalid/fetch_failed`。`updatedAt` 是最后有效快照时间，`checkedAt` 是最近检查时间。

`POST /api/subscriptions/{id}/usage` 成功返回 `{ "usage": ... }`，不返回 `saved/apply`。缺失或格式异常的响应头会更新状态并返回 200；网络/HTTP 错误返回错误状态，但保留历史有效快照。并发刷新同一订阅返回 409。用量更新不增加配置版本。


## 规则分流

`GET /api/routing` 返回设置本身，以及按当前订阅计算出的合并结果。合并报告不依赖是否已存在规则监听，便于在创建监听前先确认结果。

```json
{
  "routing": {"enabled":true,"defaultPolicy":"🌍 国外代理","mergeSubRules":true,"subRulePosition":500,
              "allowGeoRules":false,"ruleSetProxy":"DIRECT","dnsEnabled":true,
              "dnsDomestic":["https://223.5.5.5/dns-query"],"dnsForeign":["https://1.1.1.1/dns-query"]},
  "ruleSets": [{"id":"…","name":"cn-domain","policy":"🎯 国内直连","behavior":"domain","format":"mrs",
                "url":"https://…/cn.mrs","interval":86400,"noResolve":false,"enabled":true,
                "position":700,"builtin":true}],
  "policies": ["🌍 国外代理","🎯 国内直连","🛑 广告拦截","🚀 节点选择","🐟 漏网之鱼","DIRECT","REJECT"],
  "reports": [{"subscription":"机场 A","groups":4,"rules":5,"providers":1,
               "renamed":["🚀 节点选择 → [机场 A] 🚀 节点选择"],
               "droppedGeo":1,"droppedRules":2,"droppedGroups":1}],
  "groups": 10, "rules": 21, "providers": 7
}
```

`reports` 说明每个订阅贡献了什么、丢弃了什么：`droppedGeo` 是被过滤的 GEOIP / GEOSITE 规则，`droppedRules` 是语法无法解析或指向未知策略的规则，`droppedGroups` 是解析后没有任何可用成员的策略组，`renamed` 是与内置组或其他订阅重名后被改名的策略组。

规则集写入与其他配置变更一致，返回 `saved` / `apply` 包装并增加配置版本。`name` 只允许字母、数字、下划线、点和连字符，因为它同时作为磁盘缓存文件名；`mrs` 格式不支持 `classical`；`interval` 范围为 60 秒到 30 天。

`POST /api/rule-sets/refresh` 不修改任何配置，也不增加版本；配置尚未成功应用时返回 409。返回 `{"refreshed":6,"failed":[]}`，`failed` 列出拉取失败的规则集名称。

## 日志与连接

`GET /api/logs` 是轮询接口，不是流式接口。服务在内存中保留最近 2000 条内核日志，客户端携带上次的 `nextSeq` 只取增量。

```json
{"entries":[{"seq":41,"time":"2026-09-09T09:20:00Z","level":"warning","payload":"[TCP] dial …"}],
 "nextSeq":42,"level":"info","connected":true,"dropped":0}
```

`dropped` 是窗口滚动丢弃的条数，`connected` 表示与内核日志流的连接状态，`error` 在内核不可达时给出原因。单次响应最多返回 500 条。级别为 `debug/info/warning/error/silent`；`silent` 时不采集。

内核不会为成功建立的连接输出「命中了哪条规则」的日志，因此**请求走向以 `/api/connections` 为准**，日志用于看 DNS 解析、失败原因和内核事件。

```json
{"connections":[{"id":"…","network":"TCP","kind":"HTTP","source":"127.0.0.1:5000","target":"example.com:443",
                 "listener":"规则出口","listenerId":"…","port":"17892","chains":["🚀 节点选择","HK 01"],
                 "rule":"RuleSet(cn-domain)","upload":83,"download":0,
                 "startedAt":"2026-09-09T09:20:00Z","elapsedSeconds":12}],
 "downloadTotal":0,"uploadTotal":83}
```

`listener` 和 `chains` 已把内核使用的 `listener-<id>` / `node-<id>` 还原成页面上的名称；监听已被删除时按内核原值返回且没有 `listenerId`。`chains` 按从入口到出口的顺序排列。日志和连接都只存在于内存，管理服务重启后清空。

## 出口 IP 探测

探测通过被测出口自身发起一次查询，因此得到的是对端看到的地址。查询服务默认为 `https://ip9.com.cn/get`。

```json
{"nodes":{"节点 ID":{"status":"success","ip":"203.0.113.9","country":"美国","region":"加州",
                     "city":"洛杉矶","isp":"Cloudflare","asn":"AS13335",
                     "checkedAt":"2026-09-09T09:20:00Z","inFlight":false}},
 "listeners":{},"batch":{"id":"…","status":"completed","total":4,"completed":4,"succeeded":1,"failed":3},
 "service":"https://ip9.com.cn/get"}
```

状态为 `queued/running/success/failed/cancelled`。请求未能通过该出口完成时返回 `failed` 且没有 `ip`，**不会退回直连给出宿主机地址**，否则不通的节点会显示成可用。

节点探测通过一个仅监听 `127.0.0.1` 的内部端口进行，该端口绑定独立的探测策略组；探测只切换这个策略组，不影响任何承载实际流量的监听。探测串行执行并保持约 1 秒间隔，以符合查询服务每个来源地址 60 次/分钟的限制；已有探测在进行时再次发起返回错误。结果只保存在内存，管理服务重启后清空。

监听探测走该监听端口本身。对规则监听来说，得到的是**查询服务域名按当前规则实际使用的出口**；`ip9.com.cn` 是国内域名，命中国内直连时显示的就是直连地址。要确认代理节点的出口，请探测节点。


## 代理组与分类模板

以下写接口均要求现有登录会话和同源校验，返回 `saved` / `apply`，保存后增加配置版本并应用；应用失败时持久化意图仍保留为待应用。

| 方法与路径 | 请求 / 行为 |
| --- | --- |
| `POST /api/proxy-groups` | `{"name":"工作服务","kind":"select","nodeIds":[]}` |
| `PUT /api/proxy-groups/{id}` | 同上；名称不可修改，kind 可为 `select`、`url-test`、`fallback` |
| `DELETE /api/proxy-groups/{id}` | 规则集或兜底仍引用时返回 400 |
| `PUT /api/proxy-selection` | `{"name":"工作服务","member":"node-节点ID"}`；也支持该组实际包含的组名、DIRECT、REJECT；仅手动组可选 |
| `POST /api/rule-templates` | `{"ids":["category-ai-!cn","youtube"]}`；一次事务创建分类组及关联规则，冲突时整批不写入 |

`nodeIds: []` 表示跟随全部已启用、可用节点；非空数组表示显式成员，顺序用于故障转移。自动组没有可用成员时输出 REJECT。手动组始终可选默认节点组、直连与拦截。

`GET /api/state` 增加 `proxyGroups: [{id,name,kind,nodeIds}]` 和 `selections: {组名: 成员名}`。

`GET /api/routing` 保留原字段，并增加：

- `policies`：动态包含基础组、自建组和当前可用的订阅组；兜底不能指向自身“漏网之鱼”。
- `proxyGroups`：`[{name,kind,members,selected,now,chain,live,warning,customId,ruleSets}]`。成员中的 `node-<id>` 对应 state 节点；`selected` 是保存选择，`now/chain` 是已应用内核的实际出口，仅 `live:true` 时可信。没有活动规则监听、版本未应用或内核不可达时显示配置预览；失效选择保留并给出 warning。
- `proxies`：内核代理类型、当前选择和延迟历史，不含代理配置及凭据。前端可在没有管理端检测结果时使用节点历史。
- `runtimeError`：无法读取实际出口时的提示。
- `templates`：`[{id,name,description,sets}]`，每个 set 含地址、行为、格式、顺序及默认代理组。可选 id 包括 category-ai-!cn、youtube、netflix、telegram、google、microsoft、apple、steam。

规则集策略在保存时根据当前配置校验。订阅组随后消失时，规则构建将失效目标降级为 REJECT，避免整个配置失败或意外直连；管理页面显示出口失效。默认排序 300，IP 模板为 310；若人为调整过原规则优先级，需要再次核对首次命中的顺序。

选择变更通过完整配置应用事务生效，不直接暴露内核控制接口；内部出口探测组不能通过上述接口切换。选择写入生成配置的首个成员，重载与失败回滚均恢复对应配置的选择，`last-good.yaml` 可独立恢复上一版成功选择。


## 外部方案订阅和分类规则

所有接口沿用登录会话与同源校验。方案的唯一标识为 `id`；本地方案使用空字符串 `scope`。每个 scope 分别保存选择、分类覆盖、自定义条目及来源规则屏蔽记录。`GET /api/state` 增加 `routingSources`、`activeRoutingSource`、`categoryEdits`、`categoryRules`、`blockedRules`。快照正文不出现在状态响应中。

### 方案生命周期

`POST /api/routing-sources/preview` 请求：

```json
{"id":"编辑时填写","version":1,"name":"日常分流","url":"https://example.com/config.yaml","interval":86400,"autoUpdate":true,"sourceIds":[],"bindings":{"来源节点名称":"本地节点ID"},"importDns":false}
```

新建省略 id/version。响应包含 `errors`、`unresolved`、`mappings`、`ignored`、`groupNames`、`source` 和 `changed`。只有解析、绑定及校验成功才返回 `token`。失败以结构化预览返回，不写配置；单文档最大 32 MiB，最多 512 个分组。`sourceIds` 是本地节点订阅 ID，空数组表示全部来源；其中空字符串表示手动导入来源。

- `POST /api/routing-sources`：`{"token":"预览令牌"}`，保存刚刚预览的候选，令牌 10 分钟有效且一次性消费；源版本或配置版本变化后拒绝，避免提交陈旧预览。返回 `saved/changed`，新方案先保存为未启用。
- `POST /api/routing-sources/activate`：`{"id":"方案ID"}`，空 id 切回本地；验证与应用成功后才持久化切换，失败保留之前方案。
- `POST /api/routing-sources/{id}/refresh`：立即检查并更新，返回 `changed`。同源检查去重，失败记录 source.lastError，保留成功快照；未变内容不增加配置版本或重载。
- `DELETE /api/routing-sources/{id}`：仅删除非活动方案，清理该 scope 的选择与覆盖；不变更其他配置的版本。

自动检查间隔为 60 秒至 30 天；到期扫描周期 30 秒，启动后首次扫描约 1 秒。元数据包含 `version/digest/checkedAt/updatedAt/lastError/groups/rules/providers/ignored`。URL 可能含令牌，日志和摘要不应完整展示其查询字符串。

### 分类与条目

| 接口 | 请求与行为 |
| --- | --- |
| `PUT /api/categories` | `{"scope":"","edit":{"name":"原始组标识","label":"显示名","kind":"select","members":["node-ID","DIRECT"],"allNodes":true,"deleted":false}}` |
| `POST /api/categories/restore` | `{"scope":"","name":"组标识"}`；已删除组恢复删除前设置，未删除组撤销本地分组设置 |
| `GET /api/routing-entries?policy=…&q=…&offset=0` | 当前方案的规则条目，每页最多 100 条，返回 entries/total/offset/scope；policy 留空查询全部 |
| `POST /api/category-rules` | `{"scope":"","rule":{"policy":"组标识","kind":"DOMAIN-SUFFIX","value":"example.com","position":100,"enabled":true}}` |
| `PUT /api/category-rules/{id}` | 更新本地条目，未知 id 不创建新记录 |
| `DELETE /api/category-rules/{id}?scope=…` | 删除本地补充条目 |
| `POST /api/routing-entries/block` | `{"scope":"","id":"来源条目ID","text":"原始规则","block":true}`；false 恢复来源条目 |

新建分类省略 name，服务端生成稳定引用。编辑已有分类时 kind 为空表示只修改显示名称，沿用原有成员和方式。kind 支持 select/url-test/fallback/load-balance；members 可包含当前方案的组、关联节点、DIRECT/REJECT。整组删除使用 deleted:true，保留可恢复定义，规则和引用在生成阶段移除。

条目 kind 支持 DOMAIN、DOMAIN-SUFFIX、IP（也接受 IP-CIDR/IP-CIDR6），服务端规范化地址和 CIDR。编辑来源域名/IP 时，新条目携带 `replacesText`，同一数据库事务中写入补充规则并屏蔽原规则；未知的原规则不能被替换。来源条目的 id 按原规则文本摘要生成，顺序变化不会使屏蔽失效。

scope 与当前活动方案不一致时拒绝写入。分类及条目写接口返回既有 saved/apply，保存不代表应用成功；前端必须检查 apply.applied。选择接口 `PUT /api/proxy-selection` 自动使用当前方案的独立选择表。

`GET /api/routing` 增加 sources/activeSource/categoryEdits/finalPolicy，proxyGroups 增加 label/edited/ruleCount/allNodes/configuredMembers；ruleSets 返回当前方案的有效规则集入口。`POST /api/rule-sets/refresh` 仅更新当前生效配置的 HTTP 规则提供者，inline 提供者不发起网络更新。
