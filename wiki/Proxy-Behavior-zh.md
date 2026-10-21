# 代理行为

[English](Proxy-Behavior) | **简体中文**

这一页说明 webvpn 对请求和响应做了什么，以及刻意不做什么。

## 路由

按 `Host` 头路由，不按路径：

- `server.portal_host` → 门户：`/login`、`/logout`，以及 `/` 的服务索引。
- `<subdomain>.<base_domain>` → 校验会话和用户授权后，交给对应服务。
- 其他 → 404。

路径 1:1 保留，所以 `/api/x`、`/static/app.js` 和 WebSocket 端点都原样到达后端。

### 为什么用子域名而不是路径前缀

路径前缀（`/proxy/192.168.1.10/`）要改写 HTML、CSS、JS 里所有绝对 URL，还要处理 `Location`、Cookie、`Range`、gzip 和 chunked 编码。现代 SPA 基本必坏。子域名路由把成本转移到 DNS 和证书上，那是一劳永逸的配置问题。

代价是所有服务共享同一个父域：一个服务里的 XSS 可以带着用户的 Cookie 向另一个服务发请求，见 [安全模型](Security-zh)。

## 请求改写

出站请求是重新构造的。设置 `Rewrite` 钩子后，Go 会主动删掉入站的 `Forwarded`、`X-Forwarded-For`、`X-Forwarded-Host` 和 `X-Forwarded-Proto`，客户端没法把自己的值夹带进后端。

| 头 | 行为 |
|---|---|
| `Host` | 默认是目标主机。`preserve_host: true` 时改成公网 Host。 |
| `X-Forwarded-Host` | 浏览器使用的公网 Host。 |
| `X-Forwarded-Proto` | `public_scheme` 是 HTTPS 时为 `https`，尽管到后端这一跳是 HTTP。 |
| `X-Forwarded-Port` | 与 `X-Forwarded-Proto` 对应的 `443` 或 `80`。 |
| `X-Real-IP` | Cloudflare 提供的客户端地址（`CF-Connecting-IP`），不是裸连接地址。 |
| `X-Forwarded-For` | 同一个地址，`send_xff: false` 时不发。 |
| `Cookie` | 按名字删掉 webvpn 的会话 Cookie，后端自己的 Cookie 保留。 |
| `Origin` | 默认不动。`rewrite_origin: true` 时替换成后端自己算出的 origin。 |

### `rewrite_origin`

隧道回源是明文 HTTP。后端从连接推导 origin 会得到 `http://<它的主机>`，而浏览器正确发送的是 `https://<公网主机>`。校验 `Origin` 的应用会拒绝这个不一致——往往只在 WebSocket 握手上拒绝，于是页面能开，实时功能永远连不上。

`rewrite_origin: true` 把请求头改成后端自己的 origin。它同时也告诉后端一个跨站请求是同源的，而这正是 CSRF 检查依赖的信号。只对可信、且否则无法使用的服务开启。

## 响应改写

| 头 | 行为 |
|---|---|
| `Set-Cookie` | 删除 `Domain` 属性，把 Cookie 变成绑定公网子域名的 host-only Cookie。浏览器会静默丢弃 `Domain` 不是当前主机父域的 Cookie，这是设备面板"登录没反应"的常见原因。`__Host-` 前缀强制 `Secure; Path=/`；`__Secure-` 强制 `Secure`；`SameSite=None` 升级为 `Secure`。 |
| `Location` | 指向后端或 `extra_location_hosts` 里的主机时，改写成相对地址。浏览器因此留在公网主机上，不会被打到不可达的内网地址，也不会绕过登录。 |
| `Refresh` | 对老式 `Refresh: 0; url=...` 做同样处理。 |
| `Access-Control-Allow-Origin` | `fix_cors: true` 时，点名后端自身地址的值会被替换成请求的 `Origin`，并加 `Vary: Origin`。`*` 不动，也绝不会合成"带凭据的 `*`"。 |
| `Content-Type: text/html` | 强制 `Cache-Control: private, no-store`，防止 Cloudflare 把某个会话的页面发给别人。 |
| `Alt-Svc`、`Public-Key-Pins` | 删除；它们宣告的端点没人能到。 |

各开关的默认值见 [配置](Configuration-zh#services)。`rewrite_location` 默认**开启**。

## WebSocket、SSE 与长轮询

- WebSocket 升级能工作，是因为日志中间件把 `Hijack`、`Flush`、`Unwrap` 透传给了底层 writer。少了 `Hijack`，升级会报 "response does not implement http.Hijacker"，而普通请求一切正常。
- 刻意没有写超时。两个 `http.Server` 的 `WriteTimeout` 都是 0；非零值会把 SSE 和长轮询从响应中间切断，而 WebSocket 会掩盖这个 bug，因为被劫持的连接不受影响。
- Cloudflare 边缘大约 100 秒无数据就断开连接。VNC 和终端类服务需要应用层心跳。

## 修不了的部分

webvpn 不改写响应体。HTML 或 JavaScript 里写死的绝对 URL 够不到：

```html
<img src="http://192.168.1.1/logo.png">
<base href="http://192.168.1.1/">
<script>const API = "http://192.168.1.1/api";</script>
```

这些只能改后端自己的配置：

| 应用 | 设置 |
|---|---|
| Nextcloud | `overwrite.cli.url`、`overwritehost`、`trusted_proxies` |
| Home Assistant | `http.external_url`、`http.trusted_proxies`、`http.use_x_forwarded_for` |
| Grafana | `root_url` |
| GitLab | `external_url` |

用服务的 `note` 字段把这类坑记在门户卡片上。

## 服务开关速查

| 症状 | 设置 |
|---|---|
| 后端在 `X-Forwarded-For` 里看到公网 IP 就 403 | `send_xff: false` |
| 跳转打到内网 IP 或主机名 | 加进 `extra_location_hosts` |
| 页面打开但后端连不上 / 实时数据一直转圈 | `rewrite_origin: true` |
| 浏览器拒收后端 Cookie | 通常已自动处理；检查是否有 `__Host-` 要求 |
| 后端 HTTPS 用自签证书 | `insecure_tls: true`（会打警告） |
| 后端是 vhost 式，需要特定 SNI | `tls_server_name` |
| 后端期望 `Host` 是公网域名 | `preserve_host: true` |
| CORS 响应点名后端地址 | `fix_cors: true` |
| 内网索引里的卡片链接不对 | `lan_url` |

## 测试一个新服务

1. 在管理页添加（或在首次启动前写进种子配置）。
2. 打开门户点卡片，需要时登录。
3. 验证深层链接（比如 `/admin/settings`）也能用，不只看根路径。
4. 如果应用用 WebSocket，握手时同时看浏览器控制台和后端日志。
5. 如果登录看似成功又弹回来，在开发者工具里看后端的 `Set-Cookie`；多半是未剥离的 `Domain`（少见）或 `__Host-` 约束。
6. 测试时用 `webvpn -v` 打开 debug 日志。
