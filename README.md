# qq-notify-gateway

Webhook 网关：接收外部 HTTP POST 通知（文本 + base64 图片），单程序一步转换为 QQ 开放平台机器人 API v2 消息，单向推送至单聊 / 群聊。

- 纯出站调用 QQ OpenAPI，不接收 QQ 事件（openid 通过一次性 bootstrap 流程获取，见 `contrib/bootstrap-worker/`）
- AccessToken 使用 `tencent-connect/botgo` 的 token source，自动缓存与刷新
- 图片走官方推荐的分片上传（`upload_prepare` → 预签名 PUT → `upload_part_finish` → 合并 → `msg_type:7`）
- Docker 部署于 NAS，仅新增本项目容器，不改动 NAS 上其他项目

## 快速开始

```bash
cp .env.example .env   # 填入 QQ_APP_ID / QQ_SECRET / 目标 openid
docker compose up -d --build
curl -X POST http://<nas>:18080/notify \
  -H 'Content-Type: application/json' \
  -d '{"title":"测试","content":"来自网关"}'
```

详细设计见 `docs/compose/spec/qq-notify-gateway.md`。

## 配置

见 `.env.example`。三月七小助手侧使用「Webhook」渠道，body 模板：

```json
{"title":"{title}","content":"{content}","image":"{image}"}
```
