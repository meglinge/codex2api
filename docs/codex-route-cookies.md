# Codex 路由 Cookie 与按模型票据

本文记录 `X-Codex-Turn-State` 与 `__oailb` / `__cflb` 的配对方案，以及网关里已经落地的行为。提交 `2c8ca102`（`feat(codex): keep one turn-state and route-cookie pair per model`）。

## 要解决的问题

官方 Codex 客户端把两类东西一起用：

- `X-Codex-Turn-State`：上游铸造的不透明票据。同一段对话后续请求原样回带，用来做粘性路由。
- `__oailb`、`__cflb`：OpenAI / Cloudflare 的基础设施路由 cookie。官方客户端放在进程级 cookie jar 里，HTTP 和 WebSocket 共用，只存这两类基础设施 cookie，不存登录态。

codex2api 原先只按「账号 + 模型」保存并回放票据，出站请求不带 `__oailb` / `__cflb`。票据要回到铸造它的后端时，缺了这两枚 cookie，请求会落到另一台机器上。

## 定下来的模型

一个模型对应一张票据、一组 cookie。

| 键 | 票据 | Cookie |
| --- | --- | --- |
| 账号 + 模型 | 该模型保存的 `X-Codex-Turn-State` | 该模型名下还活着的 `__oailb`、`__cflb` |
| 同一模型的后续会话 | 回放这一张 | 回放这一组 |
| 另一个模型 | 用自己的票据 | 用自己的 cookie |

票据表示这一格的资格，cookie 表示把这个模型的请求送到对应后端。两者按模型放在一起，不按会话拆，也不在全账号合成一个池。

实测里，同一账号的 cookie 可以跨会话复用，而且票据和 cookie 不必来自同一次响应才能成功。产品上仍然按模型各存一份：换模型就换一对，同一模型的所有会话共用这一对。

## 保存什么

凭据键是 `credentials.codex_route_cookies`。形状是「模型 → cookie 列表」：

```json
{
  "gpt-5.6-sol": [
    {
      "name": "__oailb",
      "value": "route",
      "domain": "chatgpt.com",
      "path": "/backend-api",
      "expires": 1700003600,
      "host_only": true,
      "secure": true
    }
  ]
}
```

`expires` 是 Unix 秒。缺省或 `0` 表示没有过期时间，一直用到被换掉或被清掉。

只收这两个名字：`__oailb`、`__cflb`。登录 cookie、`__cf_bm`、`cf_clearance` 以及其它 `Set-Cookie` 一律丢掉。值最长 4096 字节，不能含空白、分号或控制字符，避免拼进 `Cookie` 头时被截断或注入。

每个模型最多 16 条，全账号最多 64 个模型（与票据的模型上限相同）。超出模型上限时，新模型的 cookie 不再写入。

进程内还有一份按账号 ID 索引的 jar。调度器用数据库快照替换内存里的 `Account` 对象时，cookie 仍留在这只 jar 上，下一次同账号同模型的请求不会丢。账号从凭据加载时，如果 jar 里还没有这个账号，就用凭据播种；jar 里已经有运行中的值时，不以较旧的数据库快照覆盖。

## 什么时候收下

每次 Codex 出站都会把「逻辑上游地址 + 本次模型」放进请求 context。收到响应后，只处理 context 里这个模型。

收下的来源：

- Codex HTTP 响应头里的 `Set-Cookie`，包括非 2xx。`doTracedUpstreamRequest` 在记录入站票据之后调用。
- WebSocket 握手响应，包括握手失败。失败的握手同样可以刷新下一条连接要用的 cookie。

逻辑地址是 `https://chatgpt.com/backend-api/codex/...`（`wss` 先收成 `https`）。Resin 把拨号 URL 改成 `http://127.0.0.1:<port>/<token>/<platform>/https/chatgpt.com/backend-api/codex/responses` 时，从路径里的 `/https/<host>/...` 抽回 chatgpt 主机，cookie 仍记在该主机上，而不是记在 `127.0.0.1` 上。

允许的主机与官方客户端一致：`chatgpt.com`、`*.chatgpt.com`、`chat.openai.com`、`chatgpt-staging.com`、`*.chatgpt-staging.com`。`api.openai.com`、明文 `http` / `ws` 不收、也不回放。

匹配规则对齐 RFC 6265 里这几条实际会碰到的约束：

- 必须带 `Secure`。
- 没写 `Domain` 时是 host-only，只回到铸造它的那台主机。
- 写了 `Domain` 时，请求主机必须等于该域或是它的子域，并且域里要有点，避免被设到 `com` 这种公共后缀上。
- `Path` 缺省时取请求路径的目录。回放时 cookie 路径必须是请求路径的前缀；路径不以 `/` 结尾时，下一个字符必须是 `/`，所以 `/backend-api` 能用于 `/backend-api/codex/responses`，不能用于 `/outside`。
- `Max-Age=0`，或 `Expires` 已经过去，删掉同名、同域、同路径的那一条。
- 同名、同域、同路径的新值覆盖旧值。只更新过期时间、值不变，内存里会续期，但不算一次需要落库的变化。

## 什么时候回放

普通 Codex 出站只有在请求里已经有 `X-Codex-Turn-State` 时，才把该模型仍匹配当前 URL 的 cookie 拼成 `Cookie` 头。没有票据的请求是在打票，带上 cookie 后上游不会给出能撑满 240 秒的票：

```text
__cflb=west; __oailb=route
```

同一路径长度按名字排序，路径更长的排在前面。

下面这些情况不附加：

- 这次请求是铸造新票据的 ping（context 带了 `WithSkipStoredCodexTurnState`）。ping 不带旧票据，也不带旧 cookie，避免把新 IP 钉回原来的后端。
- 请求体里没有模型名。
- 账号自定义头或其它调用方已经写了 `Cookie`。已有的头保持原样。
- 该模型没有还活着、且路径和主机都匹配的 cookie。

覆盖的出站路径：

- `POST /responses`（`proxy/executor.go`）
- `POST /responses/compact`
- WebSocket `/responses` 握手（`proxy/wsrelay/executor.go`）

已经握上的 WebSocket 沿用建连时的那组 cookie。下一次新建握手才带上当前模型的 cookie。连接池键没有按模型拆开，复用中的连接不会中途改写握手头。

## 换票节奏

一张健康票的可用窗口很短。同一张 292 票配上同一组 cookie，在 Oracle 上从大约 3 秒用到 191.7 秒还能打出 ASTRA；267 秒时上游把它重铸成 312，请求断开。

所以每个模型在票龄到达 **200 秒** 时就开始后台打下一张，这次用户请求仍回放旧票和它那组 cookie，不等 ping。打票请求不带旧票，也不带旧 cookie。新票被接受时，用这一轮响应里的 `Set-Cookie` 换掉该模型原来的 cookie；响应没带新 cookie，旧 cookie 也清掉。票和 cookie 一起换。票龄超过 **240 秒** 还没有新票时，旧票不再回放，请求按原来的刷新模式等待或换号。

配置里的 TTL 若短于 200 秒，以 TTL 为准。默认 43 分钟不会把一张票留到它实际失效之后。

## 什么时候换掉

cookie 不跟票一起轮换。它只在下面三种情况下变化。

1. **上游给出新值。** 该模型的 HTTP 响应或 WebSocket 握手带回了不同的 `__oailb` 或 `__cflb`，立刻替换，并异步写入凭据。
2. **整账号重置或票被丢掉。** 智力管理里的「重置缓存」、上游回带的票和管理员强制刷新，会丢掉票据，并清掉该模型绑着的 cookie。已经保存的健康票（个人整票 292 字符，里面密文 160 字节；企业整票 332 字符，里面密文 192 字节）若 Fernet 铸造时间还不到 240 秒，响应里换上来的更长票会被忽略。292 / 332 把版本、时间戳、IV 和 HMAC 都算进去了。
3. **上游自己过期。** `Max-Age` 到点后不再发送。`Max-Age=0` 马上删除。没有 `Max-Age` 也没有 `Expires` 的，一直留到被新的 `Set-Cookie` 替换或被整账号重置。

## 落库

值的身份（名字、值、域、路径、是否 host-only）发生变化才写库。续期不算。

写入走 `UpdateCredentials`，只更新 `codex_route_cookies` 这一个键，和票据落库共用 `persistMu`，避免两次读-改-写互相覆盖。写在后台 goroutine 里，超时 5 秒。账号 ID 还没有（未入库的临时对象）或进程里没装票据缓存时，只留在内存。

日志只打账号 ID 和错误，不打 cookie 值。

## 代码位置

| 文件 | 职责 |
| --- | --- |
| `auth/codex_route_cookies.go` | 按模型的 jar、解析、匹配、凭据还原 |
| `auth/store.go` | 从账号行载入 `codex_route_cookies` |
| `proxy/codex_route_cookies.go` | 出站附加、响应采集、Resin 地址还原、落库 |
| `proxy/executor.go` | HTTP `/responses` 与 compact |
| `proxy/upstream_trace.go` | HTTP 响应上的 `Set-Cookie` |
| `proxy/wsrelay/executor.go` | 握手头 |
| `proxy/wsrelay/manager.go` | 握手响应，含失败握手 |
| `proxy/codex_turn_state_cache.go` | 票据作废时连带清 cookie |

## 手动重置

智力管理矩阵里，每个账号有「重置缓存」。它调用 `POST /api/admin/codex-turn-states/reset-account`，一次清掉该账号全部模型的票据和 `__oailb` / `__cflb`，并停掉这个账号正在跑的刷新循环，避免这一轮 ping 把刚清掉的值写回去。下次请求会重新铸造。账号上运维手工粘贴的凭据级 `codex_turn_state` 不在这次清除里。

## 明确不做的事

- 不保存登录态或其它 Cloudflare cookie。
- 不把 cookie 回给下游，也不写进用量日志。
- 不在管理接口里单独展示 cookie 值；它们留在账号凭据里。
- 不用票据 TTL 当 cookie 的轮换周期。
- 不在 ping 上附带旧 cookie。
