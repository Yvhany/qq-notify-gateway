# qq-notify-gateway

Webhook 网关：接收外部 HTTP POST 通知（文本 + base64 图片），单程序一步转换为 QQ 开放平台机器人 API v2 消息，单向推送至单聊 / 群聊，并自带 Deliver 风格 Web 管理界面。

- 纯出站调用 QQ OpenAPI，主运行模式不接收 QQ 事件（openid 通过一次性 `-bootstrap` 获取）
- AccessToken 使用 `tencent-connect/botgo` 的 token source，自动缓存与刷新
- 图片走官方推荐的分片上传（`upload_prepare` → 预签名 PUT → `upload_part_finish` → 合并 → `msg_type:7`）
- **双端口**：API 口 `18080` 仅 `POST /notify`（token 校验，反代免登录放行）；
  UI 口 `18081` 承载 Web UI（数据看板 / 推送记录 / WebHook 接入 / 系统日志 /
  系统配置），由反代登录保护，两口共享记录与 WS 实时流
- 记录与日志持久化到数据卷（`./data`），WebSocket 仅在页面可见时实时推送
- Docker 部署于 NAS，仅新增本项目容器，不改动 NAS 上其他项目

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
| 消息推送记录 | 编号·内容（超10字截断+图缩略）·来源·状态，50 行/页 |
| WebHook 接入 | 端点展示、**公网域名可编辑**、body 模板一键复制 |
| 系统日志 | `data/logs/gateway.log` 尾部 + WS 实时追加 |
| 系统配置 | 只读全量配置（访问控制依赖反代登录/防火墙） |

WS 连接仅在页面可见时建立，隐藏/关闭即断开；服务端无客户端时不产生任何推送开销。

## 三月七小助手接入（Webhook 渠道）

设置页四个字段（与官方教程逐项对照）：

| 设置项 | 填写值 |
|---|---|
| 接收地址 | `http://192.168.1.21:18080/notify`（API 口；反代后改公网域名，并同步到 Web UI「公网域名」） |
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
notify_webhook_url: "http://192.168.1.21:18080/notify"
notify_webhook_body: '{"title":"{title}","content":"{content}","image":"{image}"}'
```

另需保持 `notification_enable: true` 与 `notify_send_images: true`（默认即为 true）。

## 一次性获取 openid

```bash
QQ_APP_ID=xxx QQ_SECRET=xxx ./gateway -bootstrap
# 用你的 QQ 给机器人发一条私聊 → 输出 CAPTURED type=c2c id=...
# 群内 @机器人 → CAPTURED type=group id=...
```

把抓到的 ID 填入 `.env` 的 `TARGET_OPENID`。

## 配置

见 `.env.example`。数据目录由 `DATA_DIR` 控制（compose 中为 `/data`，宿主机 `./data`）。

详细设计见 `docs/compose/spec/qq-notify-gateway.md`。
