# 排查

[English](Troubleshooting) | **简体中文**

先看日志：`journalctl -u webvpn -f`，或者在终端里跑 `webvpn -v -config config.yaml`。`-check` 能在启动前抓住配置问题；未知键是错误，不是警告。

## 登录与会话

| 症状 | 原因与处理 |
|---|---|
| 登录成功后下一步又跳回登录页 | 后端设置了 `Domain=<内网IP>` 的 Cookie；webvpn 通常会剥掉。若仍复现，检查 `__Host-` Cookie 约束或是否有同名 Cookie。 |
| 某些设备随机掉线 | Cookie 同名冲突：设备管理面板设了同名 Cookie，浏览器会同时发两个。换一个不常见的 `session.cookie_name`；webvpn 也会从转发请求里剥掉自己的 Cookie。 |
| 锁定期间正确密码也被拒绝 | 符合设计。失败锁定在服务端，持续 `login.lockout`，重复触发翻倍，按客户端地址计数。等一等、重启，或调高限制。 |
| 升级后所有人都要重新登录 | `session.signing_key` 变了，或 `session.epoch` 加过。恢复密钥，或接受重新登录。 |
| 登录页能开，POST 失败 | 检查 `public_scheme`。走隧道必须是 `https`，否则 Cookie 不带 `Secure`，重定向逻辑也会不对。 |
| 管理页登录后立刻弹回 | 打开 `/admin` 的主机名和登录时不同（IP、域名、别名）。管理页 Cookie 是 host-only，固定用一个主机名。 |
| 忘了管理员密码 | 停服务，备份 `state.json`，把某个用户的 `password_hash` 换成 `webvpn -hash` 的输出（或加 `"admin": true`），重启。确保至少留下一个管理员。 |

## 被代理的应用

| 症状 | 原因与处理 |
|---|---|
| 页面打开但按钮全失效 | Cloudflare 的 Rocket Loader 或 Auto Minify 在改写 HTML/JS。两个都关掉。 |
| 登录后后端返回 403 | 设备信任 `X-Forwarded-For` 做 IP 白名单，看到公网地址。设 `send_xff: false`。 |
| 跳转到内网 IP 打不开 | 后端返回写死的绝对跳转。把该主机加进 `extra_location_hosts`；如果写在 HTML/JS 里，去改后端配置（见 [代理行为](Proxy-Behavior-zh#修不了的部分)）。 |
| 页面能开，实时数据一直转圈 | 应用校验 `Origin`，拒绝了 HTTPS→HTTP 的不一致，通常只在 WebSocket 握手上。给这个可信服务设 `rewrite_origin: true`。`fix_cors` 治不了，它改的是响应头。 |
| WebSocket 报 "does not implement http.Hijacker" | 当前版本不应该出现；中间件会透传 `Hijack`。检查是否有别的东西包住了 ResponseWriter。 |
| SSE 卡住然后一次性全出来 | 某处引入了 `WriteTimeout`。两个 server 都必须保持 0。 |
| VNC/ttyd 一分钟左右断开 | Cloudflare 对约 100 秒无数据的连接会关闭。需要应用层心跳。 |
| 登录后反复回到登录页（Synology DSM 等） | 把 webvpn 的地址加进设备的受信任代理列表。 |
| 浏览器控制台报 CORS | 响应头点名后端地址时设 `fix_cors: true`；如果是请求被拒，看 `rewrite_origin`。 |
| 后端 HTTPS 用自签证书 | 设 `insecure_tls: true`，启动会打警告。更好的办法是给后端装受信任证书。 |

## 配置与启动

| 症状 | 原因与处理 |
|---|---|
| `unknown field "subdomian"` | `-check` 刻意严格。改正拼写。 |
| `target must not include a path` | 不支持路径前缀，映射是按子域名 1:1 的。 |
| `target is not a private address` | 目标必须是私有或回环地址。这防止 webvpn 变成开放中继。 |
| `users[0] password_hash is not a bcrypt hash` | 用 `webvpn -hash` 生成，别贴明文。 |
| `at least one user must have admin: true` | 否则状态无法管理。提升一个管理员。 |
| `session.signing_key must be at least 16 bytes` | 用 `webvpn -genkey`，密钥是 32 字节 base64。 |
| `lan_index.host is required` | 有服务指向回环。设 `lan_index.host` 或该服务的 `lan_url`。 |
| `state.path` 写入失败 / read-only file system | 默认落在配置文件旁边。生产环境设 `/var/lib/webvpn/state.json`，由单元的 `StateDirectory` 创建。 |
| 改了 `config.yaml` 的 users/services 没反应 | `state.json` 存在后这是正常的。用管理页，或删掉 `state.json` 重新播种（会丢掉管理页的改动）。 |
| `version N is not supported by this build` | 状态文件来自更新版本。换匹配的二进制或恢复备份，别盲目手改。 |

## 内网索引与管理页

| 症状 | 原因与处理 |
|---|---|
| 索引页打不开 | `ss -lntp \| grep ':80 '` 确认在监听，地址应该是内网 IP 而不是 `0.0.0.0`。查防火墙（`ufw status`）。 |
| `bind: permission denied` | 1024 以下端口，而单元没给 capability。`NoNewPrivileges=true` 下 `setcap` 静默无效；从 `AmbientCapabilities` 和 `CapabilityBoundingSet` 给。 |
| 卡片指向 `127.0.0.1`，别的机器打不开 | 回环目标没配 `lan_index.host` 或 `lan_url`。 |
| 索引页能从公网打开 | `listen` 是 `0.0.0.0`。改绑内网地址。 |
| `/admin` 返回 404 | 内网监听没开（`lan_index.listen` 为空），或者主机/端口不对。 |

## 部署与 systemd

| 症状 | 原因与处理 |
|---|---|
| 服务启动即退出，报状态写入错误 | `state.path` 指进了 `/opt/webvpn`，被 `ProtectSystem=strict` 设为只读。改成 `/var/lib/webvpn/state.json`。 |
| 服务读不到 `config.yaml` | 用会"写临时文件再改名"的编辑器（`vi`、`sed -i`）后属主变了：`chown webvpn:webvpn`、`chmod 600`。 |
| root 跑 `-check` 成功，服务以 `webvpn` 跑失败 | 服务用户读不到配置，或写不了状态目录。修属主，不是改内容。 |
| 新二进制起来后又因状态失败 | 先跑 `webvpn.new -check -config ...`；`push.py` 会自动做。保留备份。 |

## Cloudflare 与隧道

| 症状 | 原因与处理 |
|---|---|
| 隧道 `active (running)`，但面板改了不生效 | Fake-IP DNS：路由器把 `*.argotunnel.com` 解析到 `198.18.0.x`（Clash/Mihomo 默认 fake-IP 段），cloudflared 到不了边缘，配置版本冻结。试 `--protocol http2`（TCP 7844），并加直连规则（见下）。 |
| 本地解析出 `198.18.0.x` 或 `2001:2::x` | 那是代理的假 IP，不是真实地址。用 DoH 查记录：`https://1.1.1.1/dns-query?name=X&type=A`；`"Status":3` 就是 NXDOMAIN。 |
| 两层域名报 `ERR_SSL_VERSION_OR_CIPHER_MISMATCH` | Universal SSL 只覆盖一层。改成 apex 下一层，或买 ACM / 上传泛域名证书。 |
| 隧道能路由，但浏览器拒收证书 | ingress 匹配不等于证书覆盖。看证书链。 |
| 登录后的页面被发给了别人 | 给这些域名加 Cache Rule，**Bypass cache**。 |

### 让隧道流量不走代理

在透明代理的 LAN 里（Clash/Mihomo TUN 模式），隧道流量可能被绕到境外再绕回来。除了白占带宽，代理节点一挂，远程访问就全断。把下面几条放在代理规则**最前面**：

```yaml
rules:
  - DOMAIN-SUFFIX,argotunnel.com,DIRECT
  - IP-CIDR,198.41.192.0/24,DIRECT,no-resolve
  - IP-CIDR,198.41.200.0/24,DIRECT,no-resolve
```

注意：

- 域名规则在 fake-IP 模式下同样有效；代理内部记着假 IP 对应的域名。
- `no-resolve` 防止代理为了匹配 IP 规则反而去解析域名。
- 配置来自订阅的话，用客户端的覆写/mixin 功能加规则；手改会在下次更新订阅时被覆盖。
- 改 DNS 没用：TUN 模式按规则匹配，跟解析成什么地址无关。

在代理面板的 connections 页面筛 `argotunnel`，`chains` 显示 `DIRECT` 就对了。

## 还是没解决？

开 issue 时带上：

- `webvpn -check -config ...` 的输出；
- 相关日志（去掉密钥）；
- 服务配置条目（真实地址替换成占位符）；
- 失败请求时浏览器控制台和后端日志的内容。
