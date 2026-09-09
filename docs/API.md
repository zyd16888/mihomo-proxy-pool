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
| POST | `/api/subscriptions/{id}/sync` | 下载并同步订阅 |
| DELETE | `/api/subscriptions/{id}` | 删除订阅及其无引用节点 |

监听写入示例：

```json
{"name":"采集线路 A","port":17891,"nodeId":"节点 ID","enabled":true}
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
