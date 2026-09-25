// qq-openid-bootstrap — 一次性抓取 QQ 机器人的 user_openid / group_openid
//
// 背景：QQ 官方 API 的单聊/群聊目标 ID 只能从事件回调里获得。本 Worker 临时充当
// 开放平台的 webhook 回调地址：验证 Ed25519 签名（挑战握手 + 事件验签）后把
// openid 记在内存里，通过 GET <PATH_SECRET> 读取。抓取完成后应删除本 Worker。
//
// Secrets:
//   QQ_CLIENT_SECRET  机器人 AppSecret
//   PATH_SECRET       随机 URL 路径
import * as ed from '@noble/ed25519';
import { sha512 } from '@noble/hashes/sha512';

const { sign, verify, getPublicKey } = ed;

// noble v2 需要挂载 sha512 才能使用同步 API
ed.etc.sha512Sync = (...msgs) => sha512(ed.etc.concatBytes(...msgs));

const enc = new TextEncoder();

// QQ 规则：AppSecret 重复至 >=32 字节，取前 32 字节作 Ed25519 seed
function seedFromSecret(secret) {
  const s = enc.encode(secret);
  if (s.length === 0) throw new Error('QQ_CLIENT_SECRET 为空');
  const out = new Uint8Array(32);
  for (let i = 0; i < 32; i++) out[i] = s[i % s.length];
  return out;
}

function hexToBytes(hex) {
  const clean = hex.trim();
  if (clean.length % 2 !== 0) throw new Error('hex 长度非法');
  const out = new Uint8Array(clean.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(clean.substr(i * 2, 2), 16);
  }
  return out;
}

function bytesToHex(b) {
  return [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
}

function json(obj, status = 200) {
  return new Response(JSON.stringify(obj, null, 2), {
    status,
    headers: { 'Content-Type': 'application/json; charset=utf-8' },
  });
}

// 模块级内存存储：同一 isolate 内连续请求可见；wrangler tail 兜底
const store = {
  user_openid: null,
  group_openid: null,
  events: [],
  updatedAt: null,
};

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const secretPath = '/' + env.PATH_SECRET;
    if (url.pathname !== secretPath) {
      return new Response('not found', { status: 404 });
    }

    if (request.method === 'GET') {
      return json(store);
    }
    if (request.method !== 'POST') {
      return new Response('method not allowed', { status: 405 });
    }

    let seed;
    try {
      seed = seedFromSecret(env.QQ_CLIENT_SECRET);
    } catch (e) {
      return json({ error: String(e) }, 500);
    }

    const bodyText = await request.text();
    let payload;
    try {
      payload = JSON.parse(bodyText);
    } catch {
      return new Response('bad json', { status: 400 });
    }

    // 1) 配置回调时的挑战握手（plain_token + event_ts），回签 event_ts+plain_token
    const plain =
      payload?.d?.plain_token ?? payload?.plain_token ?? payload?.payload?.plain_token;
    const eventTs =
      payload?.d?.event_ts ?? payload?.event_ts ?? payload?.payload?.event_ts;
    if (plain && eventTs) {
      const signature = bytesToHex(await sign(enc(eventTs + plain), seed));
      return json({ plain_token: plain, signature });
    }

    // 2) 事件推送必须携带合法 Ed25519 签名（签名对象 = timestamp + body）
    const ts = request.headers.get('X-Signature-Timestamp') || '';
    const sig = request.headers.get('X-Signature-Ed25519') || '';
    if (!ts || !sig) {
      return new Response('unsigned request', { status: 401 });
    }
    let ok = false;
    try {
      const pub = getPublicKey(seed);
      ok = verify(hexToBytes(sig), enc(ts + bodyText), pub);
    } catch {
      ok = false;
    }
    if (!ok) {
      return new Response('bad signature', { status: 401 });
    }

    // 3) 提取 openid
    const type = payload?.t || payload?.op || 'unknown';
    const c2c = payload?.d?.author?.user_openid;
    const group = payload?.d?.group_openid;
    if (type === 'C2C_MESSAGE_CREATE' && c2c) {
      store.user_openid = c2c;
    }
    if (type === 'GROUP_AT_MESSAGE_CREATE' && group) {
      store.group_openid = group;
    }
    store.events.unshift({ type, c2c: c2c || null, group: group || null, at: new Date().toISOString() });
    store.events = store.events.slice(0, 10);
    store.updatedAt = new Date().toISOString();

    return json({ ok: true });
  },
};
