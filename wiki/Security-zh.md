# 安全模型

[English](Security) | **简体中文**

这一页说明 webvpn 保护什么、怎么保护，以及边界在哪里。

## 信任边界

```
公网 ──TLS──> Cloudflare ──隧道──> cloudflared ──明文 HTTP──> webvpn ──明文 HTTP──> 内网
                                ^ 同机、仅回环
```

公网监听只绑 `127.0.0.1`。能连到它的只有本机 `cloudflared`，所以它设置的头（`CF-Connecting-IP`、`CF-Visitor`、`X-Forwarded-Proto`）可以信任。内网其他机器既伪造不了，也看不到登录页。

内网监听是独立的：它信任内核给的 `RemoteAddr`，拒绝非私有来源，会话密钥由 `HMAC(sessionKey, "webvpn-lan-admin")` 派生。

## 传输

- `public_scheme: https` 时，公网响应带 `Strict-Transport-Security: max-age=15552000; includeSubDomains`，并把明文 HTTP 请求（包括登录 POST）以 308 跳到 HTTPS。
- 协议判断依据是 `CF-Visitor` / `X-Forwarded-Proto`，不是 `r.TLS`；回源这一跳永远是明文 HTTP。
- HSTS 要浏览器先见过一次 HTTPS 才生效，所以 Cloudflare 的 **Always Use HTTPS** 也应该开。
- TLS 在 Cloudflare 边缘终止，Cloudflare 能看到请求内容。这是使用 Tunnel 的信任前提，不是缺陷。

## 认证与授权

**登录回答"你是谁"；授权回答"你能不能到这个子域名"。** 两者分开：

- 会话 token 是无状态的：对包含用户名、过期时间、epoch 和随机 nonce 的 payload 做 HMAC-SHA256。里面没有权限集合。
- 每个代理请求都从 store 现读授权。收回授权或删用户，下一次请求就生效，不用重启，也不用等 Cookie 过期。
- `session.epoch` 是"全员下线"开关。加 1 会让所有 token 失效，代价是所有人都要重新登录。
- 密码用 bcrypt cost 12。`-hash` 从 stdin 读取，密码不会出现在 `argv` 或 shell 历史里。

## 登录防护

四层，门户和管理页各有一套独立的限流状态：

1. **失败锁定**，按客户端地址。`failure_window` 内错 `max_failures` 次就锁 `lockout`；期间没有成功登录又触发锁定则翻倍，最多 `max_lockout`。状态在服务端，刷新页面不会解除。IPv6 按 `/64` 计数。
2. **令牌桶**，按客户端地址（`attempts_per_minute`）。
3. **并发信号量**（`max_concurrent`），因为 bcrypt 比对是 CPU 炸弹。
4. **未知用户也跑一次假哈希比对**，避免用响应时间枚举出真实账号。

锁定按地址而不是用户名：否则攻击者靠猜用户名就能把真实账号锁死。代价是同一 NAT 后的客户端共享锁定，分布式攻击只能被令牌桶拖慢。

管理页登录用内核给的 `RemoteAddr`，不用转发头，因为内网任何人都能伪造。

## SSRF 与后端校验

- `target` 必须是 `http` 或 `https`，必须有主机，不能带路径。
- 字面量公网 IP 在加载配置时就被拒绝。
- 主机名在 `Dialer.Control` 里解析后、连接前校验。这同时挡住 DNS rebinding，而只做配置校验挡不住。
- 只允许私有和回环地址段（回环由 `allow_loopback_targets` 全局开关控制）。
- 目标指向 webvpn 自己的监听地址会被拒绝，避免自环。

**唯一的红线：永远不要加"代理任意 URL"的能力**（比如 `?url=` 参数）。那会把白名单变成开放的 SSRF 跳板。

## Cookie 处理

- webvpn 的会话 Cookie 在转发给任何后端之前被剥掉，设备拿不到可重放的凭据。
- 后端的 `Set-Cookie` 会删掉 `Domain`，并修复浏览器强制的 `__Host-` / `__Secure-` 前缀，见 [代理行为](Proxy-Behavior-zh#响应改写)。
- Cookie 是 `HttpOnly`、`SameSite=Lax`，`public_scheme` 为 HTTPS 时带 `Secure`。
- 会话 Cookie 的 `Domain` 是 base domain，所以它会发给每个服务子域，也会发给同一 apex 下无关的主机（比如非 HTTP 的 RTMP 主机）。token 泄露时就靠加 `epoch` 补救。

## 管理页

- 只有 `admin: true` 的用户能过登录表单。普通用户即使密码正确也是 401。
- 改动请求校验 `Origin`，缺失时退回 `Referer`，两者都必须与当前 host 一致；两个都没有就拒绝。配合 `SameSite=Lax` 构成 CSRF 防护。刻意没有每表单 CSRF token，理由写在 `sameOrigin` 的注释里。
- 管理监听是明文 HTTP，Cookie 在内网明文传输。这是为了把管理入口挡在公网之外而接受的代价。

## 日志

访问日志包含 host、method、path、status、耗时和客户端 IP，刻意不记录：

- query string（很多设备面板把 token 放在里面）；
- 任何请求头；
- Cookie 和 `Set-Cookie`；
- `Authorization`。

panic 日志带堆栈，但不带请求头。

## 已知局限

这些都是真实存在的，别假设 webvpn 能消除它们：

1. **ACL 只覆盖隧道。** 后端在内网直连可达。同网段任何人可以绕过索引和代理。ACL 的含义是"公网这条路运什么"，不是"什么看不见"。
2. **XSS 跨服务共享。** 所有服务在同一个父域下。某个服务的 XSS 可以带着受害者的 Cookie 向另一个服务发请求。不要把不可信应用挂在这套代理后面。
3. **无状态会话无法逐个吊销。** 删用户有效是因为授权每次现查；但被偷走的 token 在过期或 `epoch` 改变之前，密码学上仍然合法。
4. **Cloudflare 看到明文。** Tunnel 在边缘终止 TLS。自托管不代表 Cloudflare 看不见。
5. **内网管理页是明文 HTTP。** 同网段抓包可以偷走它的 Cookie。
6. **没有审计日志和告警。** 有日志，但没有东西检测或通知可疑行为。
7. **没有经过独立审计。** 把它当成注释写得比较认真的个人工具，而不是认证过的安全产品。

## 建议

- `listen` 保持 `127.0.0.1`，内网监听绑私有地址。
- 每个管理员用长随机密码，`admin: true` 少发。
- 保持 `public_scheme: https`，并打开 Cloudflare 的 Always Use HTTPS。
- 升级前备份 `state.json`；里面有全部密码哈希和授权。
- 怀疑签名密钥泄露就更换；有任何疑虑都可以加 `epoch`。
- 永远不要提交 `config.yaml`、`state.json`、`creds.json`。
