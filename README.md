# qq-notify-gateway
使用MimoV2.6Flash和MimoDesktop编写
> **专为 [March7Assistant](https://github.com/moesnow/March7thAssistant)（三月七小助手）优化** —— Webhook 四字段开箱即用，`{title}/{content}/{image}` 占位符直连小助手「消息推送 → Webhook」渠道，截图自动分片上传至 QQ。


Webhook 网关：接收外部 HTTP POST 通知（文本 + base64 图片），单程序一步转换为 QQ 开放平台机器人 API v2 消息，单向推送至单聊 / 群聊，并自带 Deliver 风格 Web 管理界面。

- **多通道**：一通道 = 独立入站地址 + 独立密钥 + 独立机器人 + 独立推送目标，互不干扰；
  通道在 Web UI「通道管理」页增删改，配置落盘 `data/channels.json`（0600）
- 支持两种入站契约：`generic`（`title/content/image` + `X-Gateway-Token`）与
  `fnos`（飞牛 NAS 固定 payload + `x-sac-signature` HMAC 校验）
- 纯出站调用 QQ OpenAPI，主运行模式不接收 QQ 事件（openid 通过一次性 `-bootstrap` 获取）
- AccessToken 使用 `tencent-connect/botgo` 的 token source，自动缓存与刷新（每通道独立）
- 图片走官方推荐的分片上传（`upload_prepare` → 预签名 PUT → `upload_part_finish` → 合并 → `msg_type:7`）
- **双端口**：API 口 `18080` 仅 `POST /notify` 与 `POST /notify/<通道>`；
  UI 口 `18081` 承载 Web UI（数据看板 / 通道管理 / 推送记录 / WebHook 接入 /
  系统日志 / 系统配置），由反代登录保护，两口共享记录与 WS 实时流
- 记录与日志持久化到数据卷（`./data`），WebSocket 仅在页面可见时实时推送
- 密钥类字段在 UI 中默认打码，需要真实值时单独点「显示」；SDK 调试日志已关闭，
  避免 AppSecret / AccessToken 落盘
- Docker 部署于 NAS，仅新增本项目容器（`Notify_Gateway`），不改动 NAS 上其他项目

## 快速开始

```bash
cp .env.example .env   # 填入 QQ_APP_ID / QQ_SECRET / TARGET_* 
docker compose up -d --build

# 文本
curl -X POST http://<nas>:18080/notify \
  -H 'Content-Type: application/json' \
  -d '{"title":"测试","content":"来自网关"}'

# 打开 Web UI（UI 口）
# http://<nas>:18081/
```

> 推送请求必须带请求头 `X-Gateway-Token`（值见 Web UI 系统配置页，可复制/重置）。

## Web UI

| 页面 | 说明 |
|---|---|
| 数据看板 | 今日推送量/成功率/近7日趋势（真实数据）+ WS 实时事件流 |
| 通道管理 | 每通道卡片：入站地址（一键复制）、密钥打码展示、推送目标、启停、发送测试、编辑、删除、设为默认 |
| 消息推送记录 | 编号·内容（超10字截断+图缩略）·**通道**·来源·状态，50 行/页；可按通道筛选；含保留策略（1月/3月/半年/1年/永久）、「清除30天前」与「清空全部」 |
| WebHook 接入 | **公网基地址可编辑**（每通道地址由此派生）、body 模板一键复制 |
| 系统日志 | `data/logs/gateway.log` 尾部 + WS 实时追加 |
| 系统配置 | 只读配置概览（密钥打码，访问控制依赖反代登录/防火墙） |

WS 连接仅在页面可见时建立，隐藏/关闭即断开；服务端无客户端时不产生任何推送开销。

## 多通道与入站地址

- `POST /notify` → 默认通道（兼容单通道时代的老调用方）
- `POST /notify/<通道ID>` → 指定通道
- 未知 ID 或已停用的通道返回 `404`，并写一条 `status=error` 的记录便于排查

每个通道可以绑定**自己的机器人凭证与推送目标**，因此多个通道之间不共用 QQ 的
单关系配额（20 条/分钟、1000 条/天）。首次启动若没有 `data/channels.json`，
会用 `.env` 里的 `QQ_APP_ID / QQ_SECRET / TARGET_*` 自动迁移出一条 `default`
通道，并把旧 token 原样带过去——**老部署升级后调用方无需改任何配置**。

## Webhook 测试（临时接收地址）

排查「这个服务到底发了什么」用的收件箱。Web UI 左侧「Webhook 测试」页：

- 点「生成新地址」得到一个 `8 位随机` 路径，如 `https://<你的地址>/hook/gxnpxake`
- **12 小时内有效**，到期自动停止接收；想提前换就点「作废」
- 作废后数据仍可查看，**保留 15 天**，最多回看最近 **5 个地址**
- 每个地址最多留 **50 条**，单地址正文总量上限 **1 GB**（超出淘汰最旧条目）

接任意方法、任意子路径（`/hook/<slug>`、`/hook/<slug>/a/b`、`?x=1` 都收），
不推送 QQ、不校验密钥。请求头里的 `token`/`secret`/`authorization` 会被打码，
正文按原始字节存盘（图片不截断），详情页可直接看图和下载原始报文。

> 这个地址是**公网无鉴权**的：安全性来自「8 位随机 + 12 小时窗口 + 只有你知道路径」，
> 不是来自密钥。用完就作废，别当成长期端点。

## 飞牛 NAS（fnOS）接入

飞牛官方 Webhook 只有「一个地址 + 一个密钥」，请求体是固定结构，无法自定义模板。

1. 在 Web UI「通道管理」→「＋ 新增通道」：
   - 通道 ID：`fnos`（决定入站地址 `/notify/fnos`，创建后不可改）
   - 类型：`fnos — 飞牛 NAS`
   - 签名密钥：填你在飞牛里设置的密钥（留空则该通道不校验签名，仅建议首次调试用）
   - 时间戳容差：默认 300 秒（0 = 不校验时间）
   - 填该通道要用的 QQ 机器人 AppID / AppSecret 与目标 OpenID
2. 在飞牛「系统设置 → Webhook / 通知」里：
   - 地址填 `http://<NAS内网IP>:18080/notify/fnos`
     （飞牛就跑在这台 NAS 上时可用 `http://127.0.0.1:18080/notify/fnos`）
   - 密钥填上一步的同一个值
3. 点飞牛的「测试推送」，QQ 应收到「webhook-测试推送 / 设备名称：…」的消息。

飞牛侧的签名机制（网关已按真实抓包实现）：

```
x-sac-timestamp: <unix 秒>
x-sac-signature: sha256=base64( HMAC-SHA256(密钥, 时间戳 + "\n" + 原始请求体) )
```

请求体是**紧凑 JSON**（无空格，字段顺序固定 `eventId, providerType, providerId,
eventType, occurredAtMs, level, title, body, data`）。网关校验签名与时间戳，
并按 `eventId` 去重——飞牛推送失败重试时不会重复发消息。推送内容为
`title` + 换行 + `body`，`level` 目前原样透传、不做分级过滤。

## 三月七小助手接入（Webhook 渠道）

> 走的是**默认通道**（`POST /notify`）。升级到多通道后该地址与 token 保持不变，
> 已有的小助手配置无需修改。

设置页四个字段（与官方教程逐项对照）：

| 设置项 | 填写值 |
|---|---|
| 接收地址 | `http://<你的网关地址>:18080/notify`（API 口；反代后改公网域名，并同步到 Web UI「公网域名」） |
| 请求头 | `{"X-Gateway-Token":"<系统配置页复制>"}`（必填，可在指引卡重置） |
| 请求方法 | 留空（默认 POST；网关仅接受 POST） |
| 请求头 | 留空（未启用 GATEWAY_TOKEN；Content-Type 由小助手自动补） |
| 请求体 | **必填**：`{"title":"{title}","content":"{content}","image":"{image}"}` |

> 两个坑：① 教程示例里的 `"message"` 键名不能照抄，网关只认 `title/content/image`；
> ② 请求体不配模板时带图会走 multipart（网关 400），所以必须配置模板。
> 配完点设置页「发送消息」测试，Web UI 记录页应实时出现该条记录。

等价的 config.example.yaml 写法：

```yaml
notify_webhook_enable: true
notify_webhook_url: "http://<你的网关地址>:18080/notify"
notify_webhook_body: '{"title":"{title}","content":"{content}","image":"{image}"}'
```

另需保持 `notification_enable: true` 与 `notify_send_images: true`（默认即为 true）。

## 获取 openid

**重要**：openid 是**按应用隔离**的。换一个机器人（不同 AppID）就必须重新抓 openid，
否则 QQ 会回 `11255 请求的资源不存在(用户/群已注销)`。

两种方式：

**A. Web UI（推荐）** —— 系统配置页「OpenID 采集与目标切换」：

1. 顶部「通道」下拉选中要配置的通道（用的是该通道自己的机器人凭证）
2. 点「开始监听（60 秒）」
3. 60 秒内用你的 QQ 给**这个机器人**发一条私聊（或拉它进群 / 在群里 @它）
4. 抓到后点「用作目标」，再点「保存目标」——只改当前选中通道，其他通道不受影响

**B. 命令行（一次性）**

```bash
QQ_APP_ID=xxx QQ_SECRET=xxx ./gateway -bootstrap
# 用你的 QQ 给机器人发一条私聊 → 输出 CAPTURED type=c2c id=...
# 群内 @机器人 → CAPTURED type=group id=...
```

多通道部署下推荐用 A，因为它直接写到指定通道，不用手工编辑 `channels.json`。

## 配置

`.env` 只在**首次**生成 `data/channels.json` 时作为种子（机器人凭证、目标与 token）；
之后通道配置以 `data/channels.json` 为准，请在 Web UI「通道管理」里修改，改 `.env`
不会影响已有通道。

| 变量 | 说明 |
|---|---|
| `QQ_APP_ID` / `QQ_SECRET` | 开放平台 AppID / AppSecret（仅迁移时使用） |
| `TARGET_TYPE` / `TARGET_OPENID` | `c2c` 或 `group` + 目标 openid（仅迁移时使用） |
| `LISTEN_ADDR` | API 口，默认 `:8080`（compose 映射 18080） |
| `UI_LISTEN_ADDR` | UI 口，默认 `:8081`（compose 映射 18081）；置空则禁用 UI |
| `DATA_DIR` | 数据目录，compose 中为 `/data`（宿主机 `./data`） |
| `LAN_API_BASE` | 可选。UI 里展示的「入站地址」内网前缀，如 `http://192.168.1.10:18080`；留空则用当前主机名 + `:18080` 推断 |
| `QQ_SANDBOX` / `QQ_API_BASE` | 沙箱与自定义 API 域名（仅迁移时使用） |

数据卷内容：`channels.json`（通道与密钥，0600）、`records.jsonl`（推送记录）、
`images/`、`logs/gateway.log`、`webui.json`（公网基地址与保留策略）。

详细设计见 `docs/compose/spec/qq-notify-gateway.md` 与
`docs/compose/spec/multi-channel.md`。
