---
feature: qq-notify-gateway
status: in-progress
updated: 2026-09-26
branch: feat/qq-webhook-gateway
commits:
---

# QQ Notify Gateway

## Report

## [S1] Problem

三月七小助手等本地程序需要把运行结果（文本 + 截图）推送到用户的 QQ。可选渠道中，
NapCat/Go-cqhttp 等个人号协议端被明确排除；剩余唯一能发图的合规路径是 QQ 开放平台
机器人 API v2，但它要求：AccessToken 获取与自动刷新、单聊/群聊富媒体分片上传、
openid 只能经事件获取。用户要求一个部署在 NAS Docker 上的**单程序**网关：外部只管
HTTP POST，网关一步转换为 QQ 官方 API 调用，主运行模式零接收 QQ 事件。

## [S2] Design

### 总体架构

```
调用方(三月七小助手 Webhook 渠道)
   │  POST /notify  {"title","content","image"(base64,可选)}
   ▼
qq-notify-gateway (Go 单进程, NAS Docker)
   │  出站 HTTPS → api.sgroup.qq.com（botgo token source 自动刷新 AccessToken）
   ▼
QQ 单聊/群聊  ← 文本(msg_type 0) + 截图(msg_type 7, 分片上传后发)
```

一次性 openid 获取（不进入主运行模式）：**网关内置 `-bootstrap` 开关**——临时以
botgo WS 长连接（`wss://api.sgroup.qq.com/websocket`）监听，抓到 `user_openid` /
`group_openid` 打印后进程退出；主运行模式永不监听 QQ 事件。原计划的临时
Cloudflare Worker（`contrib/bootstrap-worker/`）因所在网络对 `workers.dev` 污染
不可达而降级为备用方案（代码与已部署实例保留）。

### 入站契约

- `POST /notify`，JSON：`{"title": string, "content": string, "image": string?}`
  - `image` 为 JPEG/PNG 的 base64（容忍 `data:image/...;base64,` 前缀）；`title`、
    `content` 可为空，均为空且无图时返回 400。
  - 与三月七小助手 Webhook 渠道 body 模板 `{"title":"{title}","content":"{content}","image":"{image}"}` 一一对应。
- 可选入站鉴权：环境变量 `GATEWAY_TOKEN` 非空时要求请求头
  `X-Gateway-Token` 与其相等（401 否则）；为空则不校验（默认，LAN 部署）。
- 成功返回 200 `{"ok":true}`；QQ 调用失败返回 502 并带错误信息（调用方可见失败）。

### 配置（环境变量，`.env` 注入）

| 变量 | 必填 | 说明 |
|---|---|---|
| `QQ_APP_ID` / `QQ_SECRET` | 是 | 开放平台 AppID / AppSecret |
| `TARGET_TYPE` | 是 | `c2c` 或 `group` |
| `TARGET_OPENID` | 是 | 单聊 `user_openid` 或群 `group_openid`（bootstrap 抓取） |
| `LISTEN_ADDR` | 否 | 默认 `:8080` |
| `QQ_API_BASE` | 否 | 默认 `https://api.sgroup.qq.com`（可指向 mock 测试） |
| `QQ_SANDBOX` | 否 | `true` 时使用 sandbox API 域名，默认 `false` |
| `GATEWAY_TOKEN` | 否 | 入站鉴权，见上 |

### QQ 调用层

- **Token**：`github.com/tencent-connect/botgo/token` 的 `QQBotTokenSource` +
  `StartRefreshAccessToken`（atomic 缓存、singleflight、过期前自动刷新）——满足
  "botgo 单程序、token 自动刷新缓存"的选型要求。
- **请求**：统一自封装 HTTP 客户端，头 `Authorization: QQBot <token>`、
  `X-Union-Appid: <appid>`（与 qqbot-cloud 等已验证实现一致）；非 2xx 或响应
  `code!=0` 视为错误，错误文本含 traceID。
- **文本**：`POST /v2/users|groups/{openid}/messages`，body
  `{"content":"<title>\n<content>","msg_type":0}`（主动消息，不带 msg_id）。
- **图片（分片上传，官方推荐路径）**：
  1. `POST .../upload_prepare`：`file_type=1`、`file_size`、`file_name`、`md5`(整文件)、
     `sha1`(整文件)、`md5_10m`(前 min(size,10002432) 字节) → `upload_id`、`parts[]`
     (index 从 0、presigned_url、block_size)、`upload_config`(concurrency/retry)。
  2. 按 `parts[]` 逐片 `PUT presigned_url`（并发取 `upload_config.concurrency`，上限 4）。
  3. 每片成功后 `POST .../upload_part_finish`：`upload_id`、`part_index`、
     `block_size`、`md5`(该片 MD5)。
  4. 合并 `POST .../files`：`file_type=1, file_name, upload_id, srv_send_msg=false`
     → `file_info`（同时关注 `ttl`）。
  5. 发送：`POST .../messages`，`{"msg_type":7,"media":{"file_info":"<file_info>"}}`。
  - 单聊与群聊端点严格按 `TARGET_TYPE` 选择，两者文件互不复用（官方规定）。
  - 图片规格：png/jpg，软限 20MB；网关侧再设 20MB 上限校验，超限返回 413。
- **顺序**：先发文本消息，再发图片消息（两条主动消息；单关系 20/qpm、日上限
  1000 条，余量充足）。任一步失败即整体 502。

### 一次性 openid 获取（`-bootstrap` WS 模式；Worker 备用）

- **主路线**：`qq-notify-gateway -bootstrap`（仅需 `QQ_APP_ID`/`QQ_SECRET`）——
  botgo 事件链路建立 WS 长连接，注册 C2C/群@事件 handler；从事件帧 `d` 载荷解析
  `author.user_openid` / `group_openid`，打印 `CAPTURED type=… id=…` 后退出。
  实战约束与结论：`api.sgroup.qq.com` 与 `wss://api.sgroup.qq.com/websocket`
  直连可达；两事件共用 intent 位 `IntentGroupMessages(33554432)`。
- **备用路线**（已部署，暂不可达）：`contrib/bootstrap-worker/`（wrangler，纯 JS，
  依赖 `@noble/ed25519`）——`POST /<随机路径>` 校验 `X-Signature-Ed25519`
  （AppSecret 重复填充至 ≥32 字节作 seed，验证 `timestamp+body`）并回签
  `plain_token` 挑战；openid 存内存、`GET /<随机路径>` 读取。`QQ_CLIENT_SECRET`
  与 `PATH_SECRET` 经 `wrangler secret` 注入，不进仓库。启用前提：可达的
  回调域名（workers.dev 在当前网络被污染）。
- 已完成：`-bootstrap` 实战抓获 `user_openid=CFC6F9FF7E8132B866AF5483400C1CE2`
  并写入 NAS `.env`；用完后应从开放平台移除任何临时回调配置。

### Docker 与 NAS 部署

- 多阶段构建：`golang:1.27-alpine`（`go mod download` → `go build -trimpath`）→
  `alpine` 运行时（含 ca-certificates）；镜像内非 root 用户。
- `docker-compose.yml`：`container_name: qq-notify-gateway`、`env_file: .env`、
  端口 `18080:8080`（避开 NAS 已占用端口）、`restart: unless-stopped`、默认
  bridge 网络（**不加入任何已有项目网络**）。
- NAS 落位：仅新建 `qq-notify-gateway` 专属目录，`docker compose up -d` 只操作
  本项目；不改动其他容器、网络、卷。部署前只读核对目录与端口可用性。

### 测试边界

- 单元/集成（本地，`QQ_API_BASE` 指向 `httptest` mock）：配置解析与校验、入站
  鉴权、base64 容错与 20MB 上限、文本 payload 构造、完整分片上传流程
  （prepare→PUT→part_finish→merge→msg_type 7）与错误路径（非 2xx、code!=0）。
- 真实 API 验证（openid 抓取后）：curl 发文本 + 带图消息，以手机收到为准。
- 验证命令：`go build ./...`、`go vet ./...`、`go test ./...`。

## [S3] Out of Scope

- NapCat / OneBot / Go-cqhttp / Apprise / message-pusher 等个人号或聚合方案。
- **主运行模式**接收 QQ 事件、WebSocket、频道(guild)消息、被动回复
  （`-bootstrap` 一次性模式为唯一例外，抓完即退）。
- 消息持久化队列/重试存储（失败即 502，由调用方决定重试）。
- 多目标路由、@特定成员、markdown/键盘等富文本。
- Cloudflare 中转主链路（Worker 仅用于一次性 bootstrap）。
- NAS 上其他 Docker 项目及系统配置的任何改动。
- 三月七小助手本体的代码改动（只提供其 Webhook 渠道配置）。

## [S4] Web UI（Deliver 风格，按用户模板裁剪）

**动机**：用户提供了静态模板 `统一推送平台-Deliver风格.html` 作为视觉参考，要求
整合为网关的外部管理界面；模板中与本项目无关的概念（WebSocket 入站、签名、
渠道 B/C、消息模板、CSV 导出、ECharts CDN）一律不实现。

### 集成方式

- 前端单文件 `webui/index.html`，Go `//go:embed` 内嵌，**同端口**提供
  `GET /`（及 `/index.html`）；不新增容器、不引外部 CDN。
- 持久化目录 `DATA_DIR`（默认 `./data`，compose 中挂 `./data:/data` 并设
  `DATA_DIR=/data`）：`records.jsonl`、`images/`、`logs/gateway.log`、`webui.json`。

### 页面（左侧五项，裁自模板六项 + 新增日志）

1. **数据看板**：真实统计卡（今日推送量 / 今日成功率 / 累计记录 / 最近状态）+
   近 7 日趋势**内联 SVG 折线**（不引 ECharts）+ 实时事件流（复用 WS 推送）。
2. **消息推送记录**（用户称"消费对账记录"，高密度）：
   - 列：**编号 | 消息内容 | 来源 | 状态**（去掉"重试"列；时间以小字置于编号下）。
   - 内容截断：按 rune 取前 10 字，超出追加 `...`（三个点）；完整内容存字段供
     title 提示。
   - 图片：记录带图时，在截断文本**下方**展示缩略图（`max-width` 自适应、
     高度 auto），点击在新标签打开原图。
   - 分页：每页 50 行，紧凑行高，尽量一屏多行；支持翻页。
3. **WebHook 接入**：展示入站端点（**公网域名可编辑**——见下）、body 模板
   （一键复制）与最近事件；不展示 WebSocket 端点。
4. **系统日志**（新增页）：`logs/gateway.log` 尾部 + WS 实时追加。
5. **系统配置**（只读）：展示全部运行配置（按用户指示**不打码**，访问控制由其
   反代登录+防火墙承担）；另含版本与运行时长。

### 数据与 API 契约

- **推送记录**：`handleNotify` 每次调用写入
  `{id(自增), ts, source, status: "ok"|"error", error?, content, snippet, has_image}`；
  `source` 取入站 JSON 可选字段 `source`，缺省 `"WebHook"`。
  - 追加写 `records.jsonl`，启动时载入末尾 ≤2000 条；`id` 取历史最大值+1。
  - 带图记录把图片存 `images/<id>.<ext>`，仅保留最近 200 张（启动时裁剪）。
  - API：`GET /api/records?offset=&limit=`（新→旧）、
    `GET /api/records/<id>/image`、`GET /api/stats`
    （今日量/今日成功率/累计/最近状态/近7日每日计数）。
- **日志**：`log.SetOutput(io.MultiWriter(文件, os.Stderr))`，每行通过 WS 广播；
  文件启动时若 >32MB 则滚动为 `.1`。botgo SDK 内部 debug 不进 UI 日志。
- **公网域名（可编辑）**：`GET/PUT /api/webhook`，读写 `webui.json`
  `{"webhook_url":"https://…/notify"}`（仅用于 UI 展示/复制，默认空）。
- **系统配置**：`GET /api/config`（只读全量字段）。
- **WebSocket**：`GET /api/ws`（升级），服务端单向推送 JSON 行
  `{"type":"record"|"log"|"stats"|"webhook"|"hello", "data":…}`。
  实现约束：每连接独立发送队列 + 唯一 writer goroutine（规避 gorilla 并发写
  panic，慢客户端队列满即踢）；hello 帧在连接注册前写入。
  - 连接生命周期：前端仅在页面**可见**时保持连接，`visibilitychange` 隐藏或
    页面卸载即 `close()`；服务端读循环感知断开并注销——无人观看时无连接。
  - 初次进入以 `GET /api/logs?n=` 拉尾部，之后仅靠 WS 追加。
- **鉴权**：与 `/notify` 相同（不加 UI 专属鉴权；用户以反代登录/防火墙兜底）。
- **compose 变更**：`environment: DATA_DIR=/data` + `volumes: ["./data:/data"]`。

### 明确不做（模板有但裁掉）

WebSocket 入站、签名校验、消息模板页、渠道管理页（仅 QQ 单渠道，看板状态卡
已覆盖）、CSV 导出、ECharts/任何 CDN 依赖、在线编辑系统配置（域名除外）。

## Tasks
- [ ] T1: 网关骨架与入站层 — acceptance: 配置/env 加载、`POST /notify`（含鉴权、base64 解析、20MB 上限）有单测通过，`go vet` 干净 (covers: S2 入站契约/配置)
- [ ] T2: QQ 调用层文本推送 — acceptance: botgo token source 接入；msg_type 0 payload 构造与错误处理有 mock 测试通过 (covers: S2 QQ调用层/文本; depends: T1)
- [ ] T3: 分片上传与图片推送 — acceptance: mock 服务器上完整走通 prepare→PUT→part_finish→merge→msg_type 7，单聊/群聊路径可切换，异常路径测试通过 (covers: S2 图片; depends: T2)
- [ ] T4: Docker 构建与 compose — acceptance: `docker compose config` 校验通过；Dockerfile 多阶段、非 root、`.env.example` 齐全 (covers: S2 Docker与NAS部署; depends: T1)
- [ ] T5: 一次性 openid 获取 — acceptance: `-bootstrap` WS 模式实战抓到 c2c openid（✅ 已完成，`CAPTURED type=c2c`）；Worker 已部署保留备用 (covers: S2 一次性 openid 获取)
- [ ] T6: 本地验证 — acceptance: `go build/vet/test ./...` 全绿并记录输出 (covers: S2 测试边界; depends: T1,T2,T3)
- [ ] T7: NAS 部署与真实推送 — acceptance: NAS 上仅新增本项目目录与 `qq-notify-gateway` 容器；手机收到真实文本与截图消息 (covers: S1,S2; depends: T4,T5,T6)
- [ ] T8: 交付调用方配置 — acceptance: 给出三月七小助手 Webhook 渠道完整配置（URL/headers/body 模板）并经一次真实推送验证 (covers: S1; depends: T7)
- [ ] T9: Web UI 实现 — acceptance: 五页 + WS 实时可用；记录页四列/截断/图缩略符合 S4；域名可保存；`go build/vet/test` 全绿 (covers: S4; depends: T1,T2,T3,T6)
- [ ] T10: UI 挂卷部署与端到端验证 — acceptance: NAS 挂 `./data` 卷后真实推送实时入表且图片可点开；容器重启记录/日志/域名仍在 (covers: S4; depends: T9)
