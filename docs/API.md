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
| POST | `/api/nodes/{id}/check` | 内核延迟检测，返回 `delay` 毫秒 |
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
