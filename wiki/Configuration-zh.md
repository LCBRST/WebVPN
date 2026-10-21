# 配置

[English](Configuration) | **简体中文**

webvpn 读两个文件，搞清楚哪个文件管什么是这里最要紧的事：

| 文件 | 内容 | 谁改 |
|---|---|---|
| `config.yaml` | 监听、域名、会话、登录策略、内网索引、初始用户/服务 | 你改，然后重启 |
| `state.json` | 当前的服务清单、用户清单、按用户授权 | 内网管理页，立即生效 |

`config.yaml` 是"人写的文件"：程序从不回写，注释和属主都能保住。`state.json` 归程序管，原子写入、权限 `0600`，因为里面是 bcrypt 哈希。

未知键会直接报错。`-check` 只校验，不启动监听，也不写状态。

## `server`

| 键 | 默认值 | 含义 |
|---|---|---|
| `listen` | `127.0.0.1:8080` | 面向隧道的监听地址，`host:port`。保持回环；同机的 `cloudflared` 是唯一需要连它的客户端。 |
| `base_domain` | 必填 | Zone apex。服务地址是 `<subdomain>.<base_domain>`，会话 Cookie 的 `Domain` 也用它。 |
| `portal_host` | 必填 | 登录页和服务索引的域名，必须等于 `base_domain` 或是它的子域。通配隧道规则不匹配 apex，所以它需要单独的 DNS 记录和 ingress 规则。 |
| `public_scheme` | `https` | 浏览器使用的协议。`http` 只用于本地 hosts 测试；走隧道会让会话 Cookie 明文传输。 |
| `allow_loopback_targets` | `true` | 允许目标为 `127.0.0.0/8` 或 `::1` 的服务——后端和代理同机时这是常态。 |
| `read_header_timeout` | `10s` | 读取请求头的超时。 |
| `idle_timeout` | `120s` | 空闲连接超时。 |

这里刻意**没有 `write_timeout`**。它写死为 0：非零值会把 SSE 和长轮询从中间切断，而 WebSocket 会掩盖这个 bug，因为被劫持的连接不受它影响。

## `session`

| 键 | 默认值 | 含义 |
|---|---|---|
| `cookie_name` | `webvpn_session` | Cookie 名。用不常见的名字，避免同设备上的应用面板设出同名 Cookie。 |
| `signing_key` | 必填 | HMAC 密钥。`webvpn -genkey` 输出 32 字节的 base64；也接受至少 16 字节的原始口令。 |
| `ttl` | `168h` | 新签发 token 的有效期（7 天）。 |
| `refresh_after` | `84h` | 剩余寿命低于此值时重新签发，把固定期限变成滑动期限。必须为正且小于 `ttl`。 |
| `epoch` | `1` | 写进每个 token。加 1 并重启即可让所有历史会话失效。 |

签名密钥也可以用 `WEBVPN_SESSION_SECRET` 提供，它会覆盖文件里的值，避免密钥进备份和 shell 历史。

会话 Cookie 带 `Domain=<base_domain>`，门户和所有服务子域共享。它是 `HttpOnly`、`SameSite=Lax`，`public_scheme` 为 `https` 时带 `Secure`。

## `state`

| 键 | 默认值 | 含义 |
|---|---|---|
| `path` | 配置文件旁的 `state.json` | 存放可变状态的 JSON 文件，必须可写。 |

生产环境必须指到可写位置。systemd 单元设置了 `StateDirectory=webvpn`（创建 `/var/lib/webvpn`，属主是服务用户，权限 `0700`），而 `ProtectSystem=strict` 让 `/opt/webvpn` 只读，所以应该写：

```yaml
state:
  path: "/var/lib/webvpn/state.json"
```

目录不存在时启动会失败，错误信息会点名那个目录；正常由 systemd 单元创建。

## `users`（种子）

| 键 | 含义 |
|---|---|
| `name` | 唯一，最多 64 字节，首尾不能有空白。 |
| `password_hash` | bcrypt 哈希。用 `webvpn -hash` 生成；明文会被拒绝。 |
| `admin` | 授予内网管理页权限，并隐含拥有全部服务。状态里必须至少有一个管理员。 |
| `services` | 播种时被忽略，见下文。 |

`config.yaml` 里至少需要一个用户。如果没人带 `admin: true`，启动时第一个用户会被提升并打警告；因为没有管理员的状态在界面上无法改回来，store 拒绝持久化这种状态。

`services` 字段能被解析，但**播种时被忽略**：每个用户都会拿到全部服务，以保证升级不改变既有行为。授权之后在管理页里改。

## `login`

| 键 | 默认值 | 含义 |
|---|---|---|
| `attempts_per_minute` | `5` | 按客户端的登录 POST 令牌桶。 |
| `max_concurrent` | `1` | 同时允许的密码比对数量。bcrypt cost 12 约 250ms CPU，这一项防止突发请求打满机器。 |
| `max_failures` | `5` | `failure_window` 内错这么多次后锁定。 |
| `failure_window` | `15m` | 失败计数窗口。 |
| `lockout` | `15m` | 首次锁定时长。再次触发且期间没有成功登录则翻倍。 |
| `max_lockout` | `4h` | 翻倍的上限。 |

锁定按客户端地址、不按用户名，攻击者没法靠猜用户名把真实账号锁死。IPv6 按 `/64` 计数。状态在进程内存里，重启即清空。

## `lan_index`

| 键 | 默认值 | 含义 |
|---|---|---|
| `listen` | `""`（关闭） | 第二个监听器的 `host:port`。绑内网地址，绝不要 `0.0.0.0`。 |
| `host` | `""` | 用来替换链接里回环目标的主机名。有任何服务指向回环时必填。只能是裸主机名或 IPv4，不能带协议和端口。 |

内网监听器只提供两样东西：`/` 的免登录索引和 `/admin` 的管理页。它够不到反向代理，也够不到任何后端。低于 1024 的端口需要 `CAP_NET_BIND_SERVICE`，见 [部署](Deployment-zh#systemd)。

完整行为和注意事项见 [内网索引与管理页](LAN-Index-and-Admin-zh)。

## `services`

只有 `name`、`subdomain`、`target` 是必填的。

| 键 | 默认值 | 含义 |
|---|---|---|
| `name` | 必填 | 卡片和管理页上显示的名字。 |
| `subdomain` | 必填 | 小写 DNS 标签（`[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`），全局唯一，且不能等于门户的标签。 |
| `target` | 必填 | `http` 或 `https` URL，必须有主机，**不能带路径**。路径前缀会破坏 1:1 映射。 |
| `description` | `""` | 显示在门户卡片上。 |
| `note` | `""` | 卡片上的备注，适合记录这个服务的坑。 |
| `insecure_tls` | `false` | 跳过后端证书校验。启动时会打警告。 |
| `tls_server_name` | `""` | vhost 式 HTTPS 后端的 SNI/校验主机名。 |
| `preserve_host` | `false` | 把公网 Host 而不是目标 Host 发给后端。多数设备要默认值。 |
| `send_xff` | `true` | 发送 `X-Forwarded-For`。设备拿它做自己的 IP 白名单、看到公网地址会 403 时关掉。 |
| `strip_session_cookie` | `true` | 转发前剥掉 webvpn 的会话 Cookie。保持开启。 |
| `rewrite_location` | `true` | 把指向后端的 `Location` 和 `Refresh` 改写成相对地址。 |
| `extra_location_hosts` | `[]` | 跳转里也代表"后端自己"的额外主机名，比如 `router.lan`、`localhost:3000`。 |
| `fix_cors` | `false` | 改写点名后端自身地址的 `Access-Control-Allow-Origin` 响应头。 |
| `rewrite_origin` | `false` | 把请求的 `Origin` 改成后端自己的 origin。应用校验 Origin、因隧道协议不一致而拒绝时需要。它同时会绕过对方的 CSRF 检查，只对可信服务开启。 |
| `lan_url` | `""` | 覆盖内网索引给这个服务生成的链接。 |
| `allow_loopback` | — | 不是服务级键；由 `server.allow_loopback_targets` 全局控制。 |

### 校验规则

以下情况会拒绝该服务：

- `name` 为空。
- `subdomain` 不是小写 DNS 标签、重复，或与门户域名的标签冲突。
- `target` 不是 `http`/`https`、没有主机、带路径、指向 webvpn 自己的监听地址，或是字面量公网 IP。

主机名在之后解析，连接前会检查解析结果。这样能挡住"域名解析到公网"和 DNS rebinding，见 [安全模型](Security-zh)。

`config.yaml` 至少要有一个服务，但管理页可以删掉最后一个——空的服务列表是合法状态。

## 播种与 `state.json`

首次启动且 `state.json` 不存在时，webvpn 会按配置写出它：

- 复制所有用户和服务；
- 每个用户授予所有服务；
- 如果没人 是管理员，提升第一个用户；
- 原子写入并打日志。

之后 `config.yaml` 的 `users:` / `services:` 就被忽略。改它们不会生效，除非删掉 `state.json`（会连同管理页的所有改动一起丢掉）或者改用管理页。

`state.json` 带 schema 版本。更新版本生成的文件会被拒绝，而不是误读。升级前先备份。

## 哪些改动需要重启

| 改动 | 需要重启？ |
|---|---|
| 服务、用户、授权、密码（管理页） | 不需要 |
| `server`、`session`、`login`、`lan_index` | 需要 |
| 在 `config.yaml` 里改服务开关 | 需要；通过管理页改则不需要 |
| `session.epoch` 加 1 | 需要 |
| 更换签名密钥 | 需要（所有会话失效） |
