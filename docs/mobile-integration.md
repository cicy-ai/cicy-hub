# cicy-hub — mobile WS 接入协议

给 cicy-mobile(w-10036)。mobile 扫码后与 hub 建**一条 WebSocket**,目录 / 大聊天 /
路由全走它。连上 = 已连接。下面是逐帧协议(照着实现)。

> 现状:`/_client` WS + hubToken 鉴权已上线;目录/大聊天/send/history 全通,按 hubToken 的 `org`
> 做租户隔离(空 org = self-host,看全部)。
>
> **安全模型(重要)**:hub **不下发任何节点 api_token**。你连 `/_client` 用一个 **hubToken**;
> 够 agent(subscribe/send/history 或直连 `<team>.hub`)也用**同一个 hubToken** —— hub 关门验它,
> 节点的**拨号器在本机**把它换成本地 api_token。所以目录里**没有 `token` 字段**,节点 api_token
> 从不上网、公网一律 401。你只需持有 hubToken。

## 1. 连接 + 握手鉴权

```
wss://hub.example.com/_client?token=<hubToken>
```

- **hubToken** = `typ=hub` 的 JWT(桌面/云签发,缺口 B),hub 用 JWKS 验签。
- 握手带 query `?token=<hubToken>`(WS 惯例;也接受 `Authorization: Bearer <hubToken>`)。
- 验通过 → **101 Upgrade = 已连接**;token 无效 → 握手阶段 `401`,不升级。

## 2. 帧格式(全部 JSON 文本帧)

统一信封 `{ "type": "...", ... }`。`req_id`(客户端生成)用于请求/响应配对。

### 服务端 → 客户端

| type | payload | 说明 |
|---|---|---|
| `directory` | `{ teams: [ { team, org, agents: [Agent] } ] }` | 连上**首帧**:全量目录快照 |
| `agent_upsert` | `{ team, agent: Agent }` | 单 agent 新增/变更(status/model/context 变即推) |
| `team_offline` | `{ team }` | 某团队掉线(其 agent 全离线) |
| `chat` | `{ agent, frame: <节点 chat ws 帧原样> }` | 订阅 agent 的聊天流,**透传**节点的 `ai_chunk`/`thinking_chunk`/`status_change`/`current_updated` |
| `history` | `{ req_id, agent, turns: [...] }` | `history_req` 的响应 |
| `ack` / `error` | `{ req_id, ok:true }` / `{ req_id, error:"..." }` | 请求确认 / 出错 |

**Agent**（目录条目）:
```
{ "wid":"w-1001:main.0", "title":"知识专员", "agent_type":"cicy", "role":"master",
  "status":"idle", "model":"deepseek-v4-pro", "context_used_pct":0, "context_window":0,
  "reach_url":"https://teamA.hub.example.com" }
```
- `reach_url` = `https://<team>.hub.example.com`(该 agent 所在节点的可达地址)
- **无 `token` 字段**。够到 agent 用**你自己的 hubToken**(连 `/_client` 那个);hub 关门验它、节点拨号器在本机换成本地 api_token。节点 api_token 从不下发、公网无效。

### 客户端 → 服务端

| type | payload | 说明 |
|---|---|---|
| `subscribe` | `{ agent:"<team>.<wid>" }` | 开始接收该 agent 的 chat 流(hub 去连它节点的 chat ws 并透传) |
| `unsubscribe` | `{ agent }` | 停止该 agent 的 chat 流 |
| `history_req` | `{ req_id, agent, limit? }` | 拉历史(hub 替你去节点取 current-history) |
| `send` | `{ agent, text, submit?:true }` | 发 prompt(hub 转该节点 `/api/tmux/send`) |

`agent` 寻址一律 `<team>.<wid>`,例:`teamA.w-1001:main.0`(或省略 `:main.0` 用短 id `teamA.w-1001`)。

## 3. 四问逐条

1) **连哪 + 鉴权**:`wss://hub.example.com/_client?token=<hubToken>`;hubToken = `typ=hub` JWT(即"client 凭证"),hub JWKS 验签;101 = 已连。
2) **目录下发**:连上先一帧 `directory`(全量 snapshot),之后 `agent_upsert` / `team_offline` 增量。字段见上。
3) **大聊天**:`subscribe` 选 agent → hub 把该 agent 节点的 chat ws 帧**原样**塞进 `{type:"chat", agent, frame}` → **你现有 `ChatWsClient` 直接复用**:解一层信封,把 `frame` 喂给原有的 `ai_chunk`/`thinking_chunk`/`status_change`/`current_updated` 处理逻辑即可。
   - 历史:发 `history_req` 帧,hub 回 `history`(hub 替你去 `<team>.hub` 取 `current-history`)。**或**你走 http `GET https://<team>.hub.example.com/api/agents/current-history/<wid>?token=<hubToken>`——用**你的 hubToken**(不是节点 token,那个不存在了),hub 验完、拨号器换本地 token。推荐走 WS 帧保持"一条通道"。
4) **发 prompt**:走 WS `send` 帧(hub 转 `/api/tmux/send`);也可 http `POST <reach_url>/api/tmux/send?token=<hubToken>`,但既然一条 WS,推荐 `send` 帧。

## 4. 分工

- **hub 侧**:`/_client` WS(hubToken 握手鉴权 typ=hub)→ 推 `directory` + 管订阅 + 把节点 chat ws 透传成 `chat` 帧 + 转发 `send`/`history_req`。`/_agents`(http 快照)保留兜底。reach 一律经 hub 关门验 hubToken,**节点 api_token 不上报、不下发**(reporter 只报 agent 列表)。
- **凭证**:hubToken 签发 + 吊销 —— 自托管 hub 直接 `hub grant`(它自己的私钥签),或云端出口。QR payload `{v:1,type:hub,url:"https://hub.example.com",token:hubToken}`。
- **mobile(你)**:扫码 parse `type:hub` → 开 `/_client` WS(连上=已连)→ 收 `directory` 渲染 Drawer + 大聊天(`subscribe`/`send`/`chat`/`history`)。

A+B 好了给你 `hub.example.com` 的测试 hubToken + 一张测试 QR 联调。
