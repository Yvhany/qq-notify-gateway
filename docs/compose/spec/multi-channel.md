---
feature: multi-channel
status: delivered
updated: 2026-11-09
source: feat/multi-channel
---

# 多通道接入（飞牛 NAS Webhook → QQ）

## Report

**交付状态**：已完成并部署生产（`/vol1/1000/Docker/qq-notify-gateway`，容器 `Notify_Gateway`）。

**核心能力**

- 一通道 = 一机器人 + 一推送目标 + 一套入站鉴权 + 一条独立入站地址，互不影响。
- 两种入站类型：`generic`（`X-Gateway-Token`）与 `fnos`（`X-Sac-Timestamp` +
  `X-Sac-Signature` HMAC-SHA256）。
- 每通道独立 QQ 客户端（懒创建、可重建、故障隔离）。
- 记录/统计/Web UI 全部按通道隔离；`/notify` 保持默认通道语义，三月七小助手零改动。
- 补充：`generic` 通道兼容 FnKnock 的「标准通知 Body」（正文嵌在 `message` 段，
  `facts` 逐行追加），见 S2.10。

**生产实测**

| 项 | 结果 |
|---|---|
| 通道数 | 5 个全部加载正常 |
| 真实第三方推送 | FnKnock 1929 B 标准 body → `200 {"ok":true}`，QQ 私聊收到，记录 `status=ok` |
| 历史数据 | 重启后 `records.jsonl` 与 `images/`（58 张）无丢失 |
| 单测 | `go build ./...` / `go vet ./...` / `go test ./...` 全绿（含 4 个 FnKnock 用例） |

**与原计划的差异**

1. **T8 的「飞牛原生通道真实告警」按用户决定不做**：飞牛侧的第三方中转服务固定指向 `/notify/fnos` 且目标不可改；改为让同类第三方产品统一走
   `generic` 通道（`X-Gateway-Token`），并以 FnKnock 的真实推送作为等价验收。
2. **新增 FnKnock 兼容（S2.10）**：原 spec 未涵盖，实施中因接入 FnKnock 事件通知
   而追加，已补测试。

**遗留**

- 群聊目标受 QQ「主动消息无权限」限制（`40034105`），当前所有通道目标均为单聊 `c2c`；
  如需群聊需申请权限或改为被动回复模式。

## [S1] Problem

网关目前只能服务**一个**推送源、**一个** QQ 机器人：`QQ_APP_ID` / `QQ_SECRET` /
`TARGET_TYPE` / `TARGET_OPENID` 全部写死在 `.env`，入站只有 `POST /notify` 一种
`{title,content,image}` 契约。

现在要把飞牛（fnOS）NAS 的系统告警接进来，遇到三个具体问题：

1. **格式不符**：飞牛官方 Webhook 只有「一个 URL + 一个密钥」，请求体是固定结构
   `{eventId, providerType, providerId, eventType, occurredAtMs, level, title, body, data}`，
   没有 `content`、没有图片模板可配，与现有 `/notify` 契约不同。
2. **鉴权方式不同**：飞牛的「密钥」不是明文口令，而是 **HMAC-SHA256 签名**。已用真实
   抓包样本反推出精确方案（见 S2.3），可严格校验。
3. **单机器人有硬上限**：QQ 机器人主动消息单关系 20 条/分钟、每日 1000 条；且单实例
   只能对一个目标。用户要「多条推送互不干扰、可以接好几个机器人」。

目标：把网关从「单推送源 + 单机器人」改成**多通道**——一通道 = 一个机器人 + 一个推送
目标 + 一套入站鉴权 + 一条独立入站地址；任意通道故障不影响其他通道；现有调用方
（三月七小助手）行为零变化。

## [S2] Design

### S2.1 通道模型与配置存储

通道（channel）是本次唯一的组织单元，字段：

| 字段 | 说明 |
|---|---|
| `id` | 入站路径标识，`[a-z0-9_-]{1,32}`，唯一，不可改（改=换地址） |
| `name` | 展示名 |
| `type` | `generic`（现有契约）\| `fnos`（飞牛契约） |
| `enabled` | 停用后入站返回 404、UI 标记为停用，不发起任何 QQ 调用 |
| `token` | `generic` 型入站密钥，对应原 `X-Gateway-Token` |
| `signingSecret` | `fnos` 型签名密钥；空 = 不校验签名（首次接入可用） |
| `timestampToleranceSec` | `fnos` 型时间戳容差，默认 300；0 = 不校验时间 |
| `qq.appId` / `qq.appSecret` | 该通道自己的机器人凭证 |
| `qq.targetType` / `qq.targetOpenId` | `c2c` \| `group` 及目标 |
| `qq.sandbox` | 是否走 sandbox 域名 |

持久化：`DATA_DIR/channels.json`（容器内 `/data/channels.json`，宿主机
`./data/channels.json`），**文件权限 0600**，结构与 `webui.json` 同级，不进 git。
含密钥的字段在 UI 与日志中一律打码（与「系统配置页不打码」的旧约定冲突时，
以本特性为准：**只对 `qq.appSecret` / `token` / `signingSecret` 打码**）。

### S2.2 路由与向后兼容

- `POST /notify` → 转发到 `defaultChannel`
- `POST /notify/<id>` → 该通道
- API 端口（`:8080`）放行规则由「路径等于 `/notify`」改为「匹配 `^/notify(/[^/]+)?$`」；
  UI 端口继续屏蔽这两个路径。
- 未知 `id` → `404`，**只写系统日志、不写推送记录**。该端点无鉴权，若对任意路径
  都落记录，任何人都能匿名刷大 `records.jsonl`。
- 通道存在但 `enabled=false` → `404`，并写一条 `status=error` 的推送记录
  （属于用户可见的配置问题，需要能在 UI 里看到）。

**首次启动迁移（关键）**：`channels.json` 不存在时，用 `.env` 的
`QQ_APP_ID/QQ_SECRET/TARGET_TYPE/TARGET_OPENID` 合成一条
`{id:"default", type:"generic", enabled:true}` 通道，其 `token` 取现有
`webui.json.gatewayToken`（没有则按现有逻辑生成并持久化），`defaultChannel="default"`，
写盘后继续启动。→ 三月七小助手已配置的 `http://192.168.1.10:18080/notify` 与
`X-Gateway-Token` **完全不用改**。

`channels.json` 存在但解析失败：**不启动**（`log.Fatalf`）。静默降级会把多通道配置
悄悄变成单通道，导致推送发错人，风险高于停机。

### S2.3 飞牛（fnos）入站契约

请求：`POST /notify/<id>`，`Content-Type: application/json`，体为**紧凑 JSON**
（无空格），字段固定顺序 `eventId, providerType, providerId, eventType,
occurredAtMs, level, title, body, data`。

**签名校验**（`signingSecret` 非空时强制执行）：

```
期望头: x-sac-timestamp: <unix 秒>
        x-sac-signature: sha256=<base64>
算法:   HMAC-SHA256(key = signingSecret,
                   message = timestamp + "\n" + <原始请求体字节>)
        → base64 → 前缀 "sha256="
比较:   subtle.ConstantTimeCompare
时间:   |now - timestamp| > timestampToleranceSec → 401
```

**该方案由真实样本反推验证**，脱敏后作为黄金测试用例写进单测
（密钥与设备名已替换，签名值按替换后的报文重算）：

| 输入 | 值 |
|---|---|
| `signingSecret` | `demo-signing-secret` |
| `x-sac-timestamp` | `1794192150` |
| 原始请求体 | 323 字节紧凑 JSON（`eventId=webhook-test-1794192139319`） |
| 期望 `x-sac-signature` | `sha256=0zncyGWbOsxUhpsU+v+peM5FPI4TkKDi8oHI2Mqs3cU=` |

实现约束：
- 必须 `io.ReadAll` 保留**原始字节**用于验签，不得「先 Unmarshal 再 Marshal」。
- 验签在 JSON 解析**之前**；验签失败不写推送记录（视为攻击噪声），但写系统日志。
- 消息文本 = `strings.TrimSpace(title)` + `"\n"` + `strings.TrimSpace(body)`；
  两者皆空 → 400。
- **幂等**：按通道隔离的去重窗口，每通道最近 500 个 `eventId`。三态语义：
  收到请求先 `Claim` 占位（挡住并发重复）→ 投递 → **成功才登记为完成**，失败则释放
  占位。这样飞牛的重试能真正重新投递，而已成功的重复请求直接返回
  `200 {"ok":true,"duplicated":true}` 且不再推送。
- 响应：成功 `200 {"ok":true}`；鉴权失败 `401`；体积超限 `413`；QQ 调用失败 `502`
  并带错误文本（飞牛侧可见）。体积上限沿用现有 32MB 限制。
- `level` / `eventType` / `data` 本期**只透传不解读**（消息里不带级别前缀，按用户选择
  原样转发 `title + body`）。

### S2.4 多机器人客户端池

- 新增 `channelHub`：`map[channelID]*QQClient` + 读写锁。
- **懒创建**：首次向某通道推送时才构造 botgo token source 并
  `StartRefreshAccessToken`，避免 N 个通道 N 个刷新协程空转。
- **重建**：UI 修改某通道的 QQ 凭证/目标后，销毁该通道旧客户端（cancel 其 ctx）并按需重建；
  其他通道不受影响。
- **故障隔离**：任一通道的 token 获取或发送失败只影响该通道的响应与记录，不阻塞 others
  （各自独立请求上下文）。
- 退出时统一 cancel 所有通道 ctx。
- 现有 `NewQQClient(cfg, tokenSource, timeout, target)` 与 `targetState` 保持不变，只是从
  「全局一份」改为「每通道一份」。

### S2.5 记录与统计

- `Record` 增加 `channel` 字段（旧记录缺失 → 视为 `default`）。
- `GET /api/records?channel=&offset=&limit=`、`GET /api/stats?channel=`（空 = 全部汇总）。
- `records.jsonl` 写入新字段，向后兼容读取。

### S2.6 Web UI

新增「通道管理」页（左侧导航第 6 项）：

- 通道卡片列表：名称 / 类型 / 目标（c2c·openid 尾巴 6 位）/ 启用状态 / 入站地址（一键复制）
- 新增、编辑、删除、启用停用
- 「发送测试消息」按钮（走该通道的真实 QQ 调用）
- 密钥类字段展示为按钮触发的打码值（`••••` + 点击显示/复制）
- 推送记录页增加「通道」列与通道筛选

删除通道需二次确认；`defaultChannel` 指向的通道不允许删除（先改默认通道）。

### S2.7 NAS 部署位置

生产部署已在此特性实施期间迁移到规范目录 `/vol1/1000/Docker/qq-notify-gateway`
（原 `/home/yvhan/qq-notify-gateway` 落在系统盘，且不符合「Docker 项目统一放
`/vol1/1000/Docker/<项目名>/`」的约定）；旧目录保留为备份未删除。
端口保持 `18080`/`18081` 不变，`Port.csv` 无需新增行。

容器名由 `qq-notify-gateway` 改为 `Notify_Gateway`（compose `container_name`）。
改名会让 compose 视为不同容器，因此切换时必须先 `down` 再 `up`，否则新旧容器
会争抢 18080 端口。

### S2.8 测试边界

- **黄金用例**：S2.3 表格中的真实样本必须验签通过；改任一字节必须失败。
- 验签失败 / 缺头 / 时间戳过期 → 401；`signingSecret` 为空 → 跳过验签。
- `eventId` 重复 → 只推送一次，第二次返回 200。
- 路由：`/notify` → 默认通道；`/notify/<id>` → 指定通道；未知/停用 → 404。
- 隔离：mock QQ 下通道 A 失败不影响通道 B 的推送与记录。
- 兼容：`channels.json` 缺失时从 `.env` 迁移，`/notify` 行为与现状一致；现有
  `server_test.go` / `store_test.go` / `config_test.go` 全部保持通过。
- 命令：`go build ./...`、`go vet ./...`、`go test ./...`（在 NAS 的
  `golang:1.27-alpine` 容器内执行，`GOPROXY=https://goproxy.cn,direct`）。

## [S3] Out of Scope

- 生产目录从 `/home/yvhan/qq-notify-gateway` 迁移到 `/vol1/1000/Docker/`（另议）
- 飞牛图片推送（飞牛的 payload 不含图片）
- 飞牛 `level` 分级过滤、告警聚合、静默期
- 多用户登录/账号体系（本特性的「多」指多推送源 + 多机器人，不是多账号）
- 频道(guild)消息、markdown/键盘等富文本
- 通道级限流、失败重试队列（仍为失败即返回错误，由调用方决定重试）
- 三月七小助手本体改动

### S2.9 日志脱敏

botgo SDK 在 `Debug` 级别会打印 `getNewToken` 的请求体与响应体，内含
`clientSecret` 与 `access_token`；这些日志会写入 `data/logs/gateway.log` 并在
Web UI「系统日志」页实时展示，等于把机器人凭证摊给任何能打开 UI 的人。

处理：接管 `botgo/log.DefaultLogger`，丢弃 `Debug`/`Info`，只把 `Warn`/`Error`
带 `botgo:` 前缀转进本项目的日志。单测固化「Debug/Info 不得产生任何输出」。
存量日志中含历史凭证，部署时已轮转。

### S2.10 FnKnock「标准通知 Body」兼容

FnKnock 等第三方通知软件的标准 body 把内容嵌在 `message` 段，顶层只有
`source` / `provider_type` 之类元数据：

```json
{"context":{...},"message":{"title":"…","body_text":"…","body_markdown":"…",
 "summary":"…","severity":"info","facts":[{"label":"登录 IP","value":"…"}]},
 "payload":{},"provider_type":"webhook","source":"fn_knock"}
```

`generic` 通道原先只认顶层 `title`/`content`，遇到该格式会判「消息为空」并返回 400。

处理（`notifyRequest.normalize`）：

- 顶层字段**优先**，为空才回退：`title` ← `message.title`；
  `content` ← `message.body_text` → `message.body_markdown` → `message.summary`
  （优先纯文本，QQ 不渲染 markdown）。
- `message.facts` 以「标签：值」逐行追加，标签或值为空的行跳过。
- 顶层字段齐全时等价于空操作，原扁平契约（`{title,content,image}`）行为字节级不变。

## Tasks

- [x] T1: 通道模型与 `channels.json` 读写 + 首次启动 `.env` 迁移 — acceptance: 无 `channels.json` 时启动自动生成 `default` 通道并写盘；解析失败时拒绝启动；单测覆盖 (covers: S2.1, S2.2)
- [x] T2: 多通道路由 `POST /notify` 与 `POST /notify/{id}` — acceptance: 默认通道与指定通道路由正确，未知/停用通道 404，现有 `/notify` 测试全绿 (covers: S2.2; depends: T1)
- [x] T3: 飞牛适配器与 HMAC 验签 — acceptance: S2.3 黄金样本验签通过、改字节即失败、缺头/过期 401、eventId 幂等生效 (covers: S2.3; depends: T2)
- [x] T4: 多机器人客户端池 — acceptance: 每通道独立 QQClient 懒创建；修改 A 通道凭证不影响 B；A 失败时 B 仍正常推送（mock 测试） (covers: S2.4; depends: T1)
- [x] T5: 记录与统计按通道 — acceptance: 新记录带 `channel`；按通道过滤的记录/统计接口正确；旧记录无字段时归到 default (covers: S2.5; depends: T2)
- [x] T6: Web UI 通道管理页 — acceptance: 可增删改/启停通道，入站地址可复制，打码字段可显示，测试发送可用，记录页有通道列与筛选 (covers: S2.6; depends: T3, T4, T5)
- [x] T7: 全量验证 — acceptance: `go build/vet/test ./...` 全绿并记录输出，现有测试无回归 (covers: S2.8)
- [x] T8: NAS 部署与真实推送 — acceptance: 容器 `Notify_Gateway` 部署于 `/vol1/1000/Docker/qq-notify-gateway`；容器重启后通道配置与记录仍在；真实第三方 Webhook（FnKnock 标准通知 Body）经公网入口推送成功并在 QQ 私聊收到 (covers: S1, S2.7; depends: T7)
  - 变更：原 acceptance 的「飞牛原生 Webhook 真实告警」按用户决定不做，理由见 Report「与原计划的差异」。
- [x] T9: 文档 — acceptance: README 增补多通道与飞牛接入步骤；spec 更新为 delivered (covers: S2; depends: T8)
- [x] T10: `generic` 通道兼容 FnKnock 标准通知 Body — acceptance: 真实 1929 B 样本推送返回 200 且 QQ 收到；顶层字段优先、空值 facts 跳过、仅 facts 不为空；原扁平契约回归通过 (covers: S2.10; depends: T2)

## Journey log

- **飞牛「密钥」的实际语义**：官方 UI 只说「密钥」，实为 HMAC-SHA256 签名密钥。
  用一次真实抓包（`x-sac-signature` / `x-sac-timestamp` / 323 字节紧凑 JSON）离线
  反推确认：签名对象是 `timestamp + "\n" + 原始body`，且紧凑 JSON 的字节长度 323
  与 `content-length` 唯一吻合，排除了「带空格」与「无 data 字段」两种写法。
  这组结构因此成为回归测试的黄金用例（密钥与设备名已脱敏，签名值按脱敏报文重算）。
- **NAS 与仓库一致性核对**：NAS 生产目录不是 git clone。初次按哈希比对时 21 个 `.go`
  中有 14 个不一致，逐层排查后确认全部为 Windows `core.autocrlf` 导致的 CRLF/LF 差异，
  归一化后 21/21 内容相同，无遗留手改。结论：可以安全地以仓库 HEAD 为基线开发。
- **升级后才暴露的两个既有缺陷**：
  1. botgo 的 `Debug` 日志把 AppSecret 与 AccessToken 明文写进日志文件并实时展示在
     Web UI（见 S2.9）。这是升级前就存在的问题，因本次要动日志链路才被发现。
  2. 「公网域名」字段历史值存的是完整端点（`.../notify`），多通道后改为基地址语义，
     直接拼接会产出 `/notify/notify/<id>`。已在 `normalizePublicBase` 做兼容。
- **测试环境的平台差异**：`channels.json` 的 0600 权限断言在 Windows 上不成立
  （Go 的 `os.Chmod` 在 Windows 只映射只读位），测试按 `runtime.GOOS` 跳过；
  竞态检测（`-race`）需要 cgo，本地无 C 编译器，改在 NAS 的
  `golang:1.27-alpine` 容器（挂载只读源码 + `apk add gcc musl-dev`）里跑通。
- **独立审查发现 2 个阻断缺陷（已在 c6f9969 之后的修复提交中改正）**：
  1. **告警会被永久丢弃**：`eventId` 去重原先在「发送之前」就登记完成。投递失败回 502
     后飞牛会重试，但重试命中去重表被判为重复，直接返回 `200 duplicated` —— 与
     「502 让飞牛重试」自相矛盾。改为三态 `Claim/finish`：占位 → 投递 → 成功才登记
     完成，失败则释放占位。已在 NAS 上用「APIBase 指向不可达地址」的探针通道实测：
     同一 eventId 连续两次都回 502（真重试），不再被吞。
  2. **升级会静默发错人**：迁移只读了 `.env`，丢掉了 `webui.json` 里的推送目标；
     而单通道时代启动逻辑恰恰是用 `webui.json` 覆盖 `.env` 的。老部署若在 UI 里
     改过目标，升级首启会被改回 `.env` 初值。现按同一优先级迁移，并在迁移完成后
     清空 `webui.json` 里已失效的 token 种子（否则它是个「复活旧 token」的暗坑）。
  另修三处：去重窗口改为按通道隔离（原先 A 通道的 eventId 会吞掉 B 通道的同名事件）、
  时间戳容差 0 不再被 `normalize` 改写为 300、未知通道不再落记录。
- **教训**：这两个缺陷都不是「代码写错」，而是**契约没测到**——缺了「投递失败后重试」
  与「跨通道同名事件」两条用例。幂等/去重的登记时机必须绑定「投递成功」这个事实，
  而不是「收到请求」这个动作。
- **第三方通知软件的 body 形态差异（接入 FnKnock 时踩到）**：FnKnock 的「标准通知
  Body」把正文嵌在 `message.body_text`，顶层只有 `source` / `provider_type`。网关
  只认顶层 `title`/`content`，于是所有环节（路由、token 鉴权、JSON 解析）都正确，
  却在最后一步判「消息为空」返回 400，看起来像鉴权问题。定位靠的是推送记录里那条
  `status=error, error="title、content、image 不能全部为空", source=fn_knock`——记录
  写明 token 已通过（401 不会落记录），才把嫌疑从鉴权转到字段位置。
  教训：接第三方推送前先拿到**真实 body 原文**再写映射，不按字段名想当然。
  另：该产品的 `X-Fn-Knock-Signature` 共享密钥未实现校验，兼容按「token 鉴权 +
  信任来源」处理，已在此说明以免误以为有签名保护。
