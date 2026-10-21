# webvpn

在浏览器里打开一个网址，就能访问内网的 Web 服务（路由器后台、NAS 面板、Grafana……）。不需要装客户端，不需要在每台设备上配代理，不需要公网 IP。

A self-hosted, login-protected reverse proxy for LAN web apps, exposed through a Cloudflare Tunnel. Subdomain routing, stateless signed cookies, per-user access control, and a LAN-only admin page.

```
浏览器 ──HTTPS──> Cloudflare 边缘 ──隧道──> cloudflared ──HTTP──> webvpn ──> 内网服务
       (外网)                    (TLS 在此终止)   (内网机器)      (:8080)    (192.168.x.x)
```

## 设计取舍

**用子域名路由，不是路径。** `https://app-vpn.example.com/` 一对一映射到 `http://192.168.1.10/`，所以**完全不需要改写 HTML/CSS/JS**。相对路径、脚本里的绝对路径、WebSocket 端点全都自动正确。

换成 `/proxy/192.168.1.10/` 这种路径形式，后端返回的 `/login.js`、`/api/x`、`Location` 跳转、Cookie 路径全都指错地方，必须做全文改写（还要处理 gzip/chunked/range/编码），现代 SPA 基本必坏。代价是路径模式下什么都不用配，子域名模式需要泛域名 DNS 和证书。

**无状态签名 Cookie。** 重启不掉线。代价是**会话**本身没法逐个吊销——要全员下线就改 `session.epoch` 再重启。但**权限**是可变的：token 里只存用户名、不存权限，每个请求现查一遍，所以收回某个人对某个服务的访问（或者直接删掉账号）立刻生效，不用等他手里的 Cookie 过期。

**可变的那部分状态单独存一个 `state.json`，不回写 `config.yaml`。** 服务清单、用户、访问权限这些会在浏览器里改的东西，写在 `/var/lib/webvpn/state.json`（systemd 的 `StateDirectory` 管着，0700）。`config.yaml` 保持「人写的文件」这个身份：注释不会被程序抹掉，属主也不会被写操作弄乱。

## 快速开始

```bash
go build -o webvpn .

./webvpn -genkey                  # 生成 session 签名密钥
printf 'yourpassword\nyourpassword\n' | ./webvpn -hash   # 生成 bcrypt 哈希

cp config.example.yaml config.yaml
$EDITOR config.yaml               # 填入密钥、哈希、base_domain、服务列表

./webvpn -check -config config.yaml   # 校验，启动前就能发现问题
./webvpn -config config.yaml
```

本地测试不用碰 Cloudflare：把 `public_scheme` 设成 `http`，在 hosts 里把子域名指向 `127.0.0.1`，用浏览器打开即可。

## 项目结构

```
main.go                 进程入口：-check / -hash / -genkey
internal/config/        YAML 解析与校验（未知键直接报错）
internal/store/         state.json：用户、服务、访问权限
internal/auth/          登录、签名 Cookie、失败锁定与限流
internal/proxy/         反向代理、Location / Cookie / CORS 改写
internal/portal/        门户与管理页模板
internal/app/           路由装配、两个监听器、中间件
deploy/                 systemd 单元与 cloudflared 示例
push.py                 一键部署脚本（依赖见 requirements.txt）
```

Go module 路径是 `github.com/LCBRST/webvpn`。

## 部署

交叉编译出 Linux 二进制：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o webvpn-linux-amd64 .
```

拷到机器上，配好 `config.yaml`，用 `deploy/webvpn.service` 注册成 systemd 服务（需要先建一个 `webvpn` 系统用户）。

单元文件里的 **`StateDirectory=webvpn`** 负责建出 `/var/lib/webvpn` 并授权给服务用户，`state.json` 就落在那里。**别把 `state.path` 指到 `/opt/webvpn`**：`ProtectSystem=strict` 让那个目录是只读的，服务会在启动写状态时失败退出，而报错看着像权限问题、不像路径问题。

`push.py` 是一键部署脚本（编译 → 备份 → 上传 → 拿**服务器上现有的**配置跑 `-check` → 补齐缺失的配置段 → 换单元和二进制 → 重启 → 验证监听地址、索引页、管理页、状态文件）。它**只往配置里追加、绝不覆盖**，也不会碰 `state.json`。

用之前先 `pip install -r requirements.txt`，并准备一个 `creds.json`（格式见脚本顶部注释，已被 `.gitignore` 忽略），或者用 `WEBVPN_CREDS` 指向别处的凭据文件。任何一步失败都会自动回滚到带时间戳的备份。

cloudflared 侧见 `deploy/cloudflared.example.yml`：

```bash
cloudflared tunnel login
cloudflared tunnel create webvpn
cloudflared tunnel route dns webvpn "vpn.example.com"
cloudflared tunnel route dns webvpn "*-vpn.example.com"
cloudflared tunnel ingress validate
```

## 修改配置

配置分两半，这是这份文档里最容易搞混的一件事：

| 改什么 | 在哪改 | 生效方式 |
|---|---|---|
| **服务清单、用户、谁能访问哪些站点** | 内网管理页 `http://192.168.1.50/admin` | 立即，**不用重启** |
| **其余全部**（`server` / `session` / `login` / `lan_index` / 每个服务的开关） | `config.yaml` | 重启 |

管理页改的落在 `/var/lib/webvpn/state.json`，**不碰 `config.yaml`**（保住那一堆注释，也不踩属主坑）。`config.yaml` 里的 `services:` / `users:` 从此**只是首次播种用的种子**——`state.json` 一存在，一切以它为准。所以从管理页删掉的用户不会因为重启而复活，反过来，往 `config.yaml` 里加个用户再重启也不会生效。播种规则和这条边界见下面「内网管理页」。

`config.yaml` 在部署目录里（systemd 单元里是 `/opt/webvpn/config.yaml`，mode 600）。**这一半没有热加载**，改完必须重启：

```bash
cp config.yaml config.yaml.bak
vi config.yaml
./webvpn -check -config config.yaml    # 必须打出 config ok
chown webvpn:webvpn config.yaml && chmod 600 config.yaml
systemctl restart webvpn
```

**那行 `chown`/`chmod` 不能省。** `vi`、`sed -i` 这类工具是「写临时文件再改名」，会把属主变成 `root:root`。文件一旦不是 `webvpn` 可读（600 + root 属主），重启时服务读不到配置，直接启动失败——报错是权限问题，不是配置问题，容易查错方向。

**`-check` 是安全网。** 配置里没见过的键**直接报错**而不是静默忽略，所以打错一个字母不会退化成「改了没反应」：

```
webvpn: config.yaml: services[0]: unknown field "subdomian"
```

**日常加服务别改这个文件**，去管理页点一下就行。下面这段是给「管理页还没起来」或者「想把它写成初始状态」准备的。**加一个服务最少三行**（只有 `name` / `subdomain` / `target` 是必需的，其余全有默认值）：

```yaml
  - name: "Ollama"
    subdomain: "ollama"
    target: "http://192.168.1.50:11434"
```

`subdomain` 必须是不带点的合法 DNS 标签、全局唯一、且不能等于 `portal_host` 的标签（那是门户自己）。可选的还有 `description` / `note`（显示在门户卡片上）、`insecure_tls`、`preserve_host`、`send_xff`、`extra_location_hosts`、`fix_cors`、`rewrite_origin`（后两个见排查表）。

| 改动 | 要不要动 Cloudflare |
|---|---|
| 加/删服务 | 隧道用的是**通配 ingress** 就不用；逐条列 public hostname 的就要补一条 |
| 改 `listen` 端口 | 要，隧道那条 ingress 的 `service` 跟着改 |
| 改 `base_domain` / `portal_host` | 要，DNS 记录 + public hostname。且注意上面证书那条约束——服务名必须是一层 |
| 改密码 / 加用户 / 分配权限 | **别动这个文件**，去内网管理页。写在这里只在首次播种时才有用 |
| 调限流 / 改 `epoch` / 改任何 per-service 开关（如 `rewrite_origin`） | 不用，只动这一个文件，然后重启 |
| 改 `lan_index` | 不用，它压根不走 Cloudflare。但动到 1024 以下的端口要同步改 systemd 单元 |

**收回访问和踢下线是两件事，别混。** 会话是无状态签名 Cookie，不进内存，所以 `systemctl restart` **不会**把人踢下线——这是当初选无状态方案换来的主要好处。要**收回某个人对某个服务的访问**，在管理页里取消勾选、或者直接删掉账号，他下一次请求就 403，不用重启，他手里那个 Cookie 还有效也没用（授权是每次请求现查的，见「安全模型」）。要**让所有人重新登录**——比如怀疑某个 token 泄露了——才需要把 `session.epoch` 加 1 再重启。

密码永远走 stdin 生成哈希，不要手写：

```bash
./webvpn -hash        # 交互输两遍，把输出的 $2a$12$... 粘到 password_hash
```

## 内网索引页与管理页（可选）

在屋子里访问服务不该绕隧道——手机出门绕一圈 Cloudflare 再回来，就为了打开一个隔一个网段的页面，还要输密码。配了 `lan_index` 之后，浏览器打开 `http://192.168.1.50/` 就是一张卡片列表，链接指向各服务的内网直连地址，不要登录：

```yaml
lan_index:
  listen: "192.168.1.50:80"
  host: "192.168.1.50"
```

它是**第二个监听器**，和 18790 那个完全分开：那边有反代和门户会话，这边只有两样东西——免登录的服务索引，和要登录的管理页。**它够不到 proxy，也够不到任何后端**，这是刻意这么写的，不是顺手：`lan_index.listen` 绑哪个地址，就只有那个网段能碰到这两个页面。

| 键 | 说明 |
|---|---|
| `listen` | 留空即关闭（默认）。**绑内网 IP，别写 `0.0.0.0`**：这台机器有公网地址，`0.0.0.0` 等于把服务清单发给全互联网。代码里还有一道「只放行私有来源」的兜底，但那只是兜底，真正的控制项是这个绑定地址 |
| `host` | 只在有服务的 `target` 是回环地址（`127.0.0.1`）时才需要。内网浏览器点 `127.0.0.1` 会打到**它自己**，所以这种情况必须换成能连的地址（端口保留）。有回环 target 却没配 `host`，**启动直接报错**，不给你一页死链 |
| 服务的 `lan_url` | 个别服务想单独指到别处时用它覆盖 |

**低于 1024 的端口需要 capability。** 单元文件里必须有这两行，`CapabilityBoundingSet` 不能省——不写的话 ambient 那个又会被剥掉：

```ini
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
```

**别想着改用 `setcap`。** 单元里有 `NoNewPrivileges=true`，内核会直接忽略文件 capability，`setcap` 是**静默失效**——你会得到一个看起来设过权限、实际起不来的二进制。

**索引页没有登录。** 同网段任何设备都能看到这份清单（只有服务名和描述，没有凭据，也没有代理能力）。这本来就是"快速索引"的前提，介意就别开。

### 管理页

同一个监听器上，`http://192.168.1.50/admin` 是管理页：加删站点、加删用户、改密码、**按用户勾选他能访问哪些站点**。改动立即生效，不用重启。

- **要登录，而且只有 `admin: true` 的账号能进。** 普通账号在登录表单就会被挡下来（401），不给一个进去什么也做不了的壳。
- **首次启动会自举一个管理员。** 老配置里没有 `admin:` 这个字段，而一个没有管理员的状态在界面上没法改回去，所以启动时会把**第一个用户**提成管理员，并打一条 `WARN`。去看一眼日志、在管理页里改成你想要的那个账号。
- **播种规则是「不收回」**：`state.json` 第一次生成时，每个用户都拿到**全部**站点。升级不该让人突然用不了东西。收紧是在管理页里做的，一步一步来。
- **管理员隐含拥有全部权限**，勾选框对他只是记录。所以管理页上「谁能访问什么」的勾选状态显示的是**存下来的名单**，不是实际权限——不然管理员把某个站点取消勾选、再一保存，就会在不知道的情况下把所有人都授权了。

**「删除」是立刻生效的，别指望 token 过期。** 会话 Cookie 是无状态签名，删用户不会让它失效；真正拦住他的是「每次请求现查一遍权限表」。所以删掉账号之后，他手上那个仍然合法的 Cookie 立刻什么都打不开。

**局限，两个都要知道：**

- **ACL 只管隧道那条路。** 后端自己就在局域网上开着（`192.168.1.50:9191` 这种），同网段的人绕过索引页直接敲地址就行，谁也拦不住。所以权限的语义是「**经隧道从公网进来这条路对你设防**」，不是「这个服务你看不到」。内网索引页因此照旧列全部——列不列都拦不住。
- **这个端口是明文 HTTP。** 管理页的会话 Cookie 在局域网里是明文传输的，同一网段能抓包的人就能拿到。这是"只在内网开放"这个选择的固有代价。介意的话只能给这个端口上自签 TLS，或者把管理页挪到隧道侧（但那就不满足"仅内网"了）。

管理页一旦被拿下等于整套 VPN 的控制权。所以它要登录、要限流、改动请求要过 `Origin` 检查，`admin` 这个标记别随手发。

## ⚠️ 证书：选域名之前必读

Cloudflare 免费版 Universal SSL 只覆盖 **`example.com` 和 `*.example.com`，即恰好一层**。

- `app-vpn.example.com` → 一层，**免费证书覆盖** ✅
- `app.vpn.example.com` → 两层，**签不出证书** ❌ 浏览器报 `ERR_SSL_VERSION_OR_CIPHER_MISMATCH`

坑在于 **cloudflared 的通配 ingress 会照样路由两层域名**，所以"隧道通了"不能说明问题。要用浏览器打开看证书链。

想用两层的 `app.vpn.example.com`，要么买 Advanced Certificate Manager（$10/月，自动签 `*.vpn.example.com`），要么上传自签的通配符证书（90 天要续）。改用一层只需要把 `base_domain` 从 `vpn.example.com` 改成 `example.com`，再把 `subdomain` 值调整一下。

## Cloudflare 后台还要配的几项

这些不在配置文件里，但漏了会出怪问题：

| 设置 | 值 | 不配会怎样 |
|---|---|---|
| Cache Rule | 对这些主机名 **Bypass cache** | 一个会话登录后的页面被缓存下来回给另一个请求 |
| Rocket Loader | **关** | 延迟执行内联脚本，老设备后台表现为"按钮点了没反应" |
| Auto Minify | **关** | 破坏对空白敏感的模板 |
| Email Obfuscation | **关** | 把页面里的邮箱替换成加密脚本 |
| Always Use HTTPS | **开** | — |

代码里也做了兜底：`text/html` 一律强制 `Cache-Control: private, no-store`。

## 已知边界

**后端 HTML 里写死的绝对 URL 修不了。** `<img src="http://192.168.1.1/logo.png">`、`<base href="http://192.168.1.1/">`、JS 里硬编码的 API 基址，这些都只能改后端自己的配置：

- Nextcloud：`overwrite.cli.url`、`overwritehost`、`trusted_proxies`
- Home Assistant：`http.external_url`、`http.trusted_proxies`、`http.use_x_forwarded_for`
- Grafana：`root_url`
- GitLab：`external_url`

每个服务在配置里有个 `note` 字段，适合把这类坑记在门户上。

**不要在这套东西后面挂不可信的应用。** 所有服务都在同一个站点（`*.example.com`）下，一个服务里的 XSS 可以带着 Cookie 向其他服务发请求。

## 排查

| 症状 | 原因 |
|---|---|
| 页面打开但按钮全失效 | Cloudflare 的 Rocket Loader / Auto Minify 在改 HTML，关掉 |
| 在某些设备上随机掉线 | Cookie 同名冲突。设备的 admin 面板设了同名 Cookie，浏览器会同时发两个，`r.Cookie()` 只返回第一个。用不常见的 `webvpn_session` 名字可避免；已默认从转发请求里剥掉 |
| 登录后刷新又跳回登录页 | 后端给 Cookie 设了 `Domain=192.168.x.x`，浏览器拒收。代码已自动清掉 Domain，若仍复现，检查后端是否用了 `__Host-` 前缀 |
| 登录后访问设备返回 403 | 设备信任 `X-Forwarded-For` 并用自己的 IP 白名单。把该服务的 `send_xff` 设成 `false` |
| 跳转到内网 IP 打不开 | 后端返回写死的绝对跳转。把该跳转的域名加进 `extra_location_hosts` |
| 前台页面能开，但后台连不上、实时数据一直转圈 | 应用**自己**在校验请求的 `Origin`。隧道回源是明文 http，应用据此把自己算成 `http://<它收到的 Host>`，跟浏览器的 `https://<公网域名>` 对不上就拒绝——而且往往只拒 WebSocket 握手，于是页面照常渲染、看着像"开着"。给这个服务加 `rewrite_origin: true`。注意 `fix_cors` 治不了它：那个改的是**响应**头 `Access-Control-Allow-Origin`，这里卡的是**请求**头 |
| VNC / ttyd 一分钟就断 | Cloudflare 侧对无数据流动的连接约 100 秒超时，需要应用层心跳 |
| SSE 卡住然后一次性全出来 | 确认没有给 `WriteTimeout` 设值（必须是 0） |
| 二级子域名证书错误 | 见上面的证书一节 |
| 隧道 `active (running)`，但 Cloudflare 面板改了不生效 | 假 IP DNS：路由器把 `*.argotunnel.com` 解析到 `198.18.0.x`（Clash/Mihomo fake-IP 的默认段），cloudflared 一直拨不到边缘，日志里是 `no free edge addresses left to resolve to`，配置版本号从此冻结不动。先加 `--protocol http2` 改走 TCP 7844 让它通，根治办法见下面「让隧道流量不走代理」 |
| 本地解析出 `198.18.0.x` 或 `2001:2::x` | 那是代理的 fake-IP，不是真实地址，本地测什么都不算数。要判断某条记录到底存不存在必须走 DoH：`https://1.1.1.1/dns-query?name=X&type=A`，`"Status":3` 就是 NXDOMAIN |
| 内网索引页打不开 | 先 `ss -lntp \| grep ':80 '` 确认在监听（地址应该是内网 IP，不是 `0.0.0.0`），再 `ufw status` 确认放行了 |
| `webvpn` 起不来，日志是 `bind: permission denied` | 绑了 1024 以下的端口，而 systemd 单元没给 `CAP_NET_BIND_SERVICE`。注意 `setcap` 在 `NoNewPrivileges=true` 下是静默无效的，必须从单元给 |
| 启动报 `lan_index.host is required` | 有服务的 `target` 是回环地址。内网浏览器点 `127.0.0.1` 会打到它自己，所以要么配 `lan_index.host`，要么给那个服务单独设 `lan_url` |
| 内网索引页的某张卡片点开连不上 | 那个服务只监听 `127.0.0.1`，或只听在某个内网浏览器够不到的地址上。用 `lan_index.host` 或该服务的 `lan_url` 指到能连的地址 |
| 改了 `config.yaml` 的 `services:` / `users:` 但没反应 | **正常，不是 bug。** `state.json` 一存在，`config.yaml` 里的这两节就只是历史的种子。要改去管理页。真想推倒重来就停服务、把 `/var/lib/webvpn/state.json` 挪走、再启动——但那会连同管理页里做过的所有改动一起丢掉 |
| 服务起不来，日志里是写 `/opt/webvpn/state.json` 失败（read-only file system / permission denied） | `state.path` 没配，默认落在只读的 `/opt/webvpn`。改成 `/var/lib/webvpn/state.json`（`push.py` 会自动补这一段） |
| 某个账号登录得上，一开服务就是 403 | 他在那个服务的授权名单里没有。管理员去管理页勾上。管理员自己不会 403 |
| 登录了管理页，但下一步就弹回登录页 | 换地址打开了。管理页的 Cookie 故意不写 `Domain`，所以 `192.168.1.50` 和别的主机名/别名是两个 Cookie 域，等于两个会话 |
| 管理员密码忘了，或者进不去管理页了 | 状态里有「至少一个 admin」的硬约束，界面上删不掉最后一个，所以不会把自己锁死；锁死的是密码。停服务，编辑 `/var/lib/webvpn/state.json` 里某个用户的 `password_hash`（用 `./webvpn -hash` 生成），或者把 `"admin": true` 加上，再启动。**改之前先备份那个文件** |

### 让隧道流量不走代理

这台机器如果处在被透明代理的 LAN 里（Clash/Mihomo 的 **TUN 模式**最常见），隧道流量会被绕到境外再绕回来。代价不只是白占订阅流量：**代理节点一挂，外网就再也进不来内网**——而这段流量本来就只是「内网机器 → Cloudflare 边缘」，压根不该出国。

根治办法是在 Clash 的 `rules` **最前面**加：

```yaml
rules:
  - DOMAIN-SUFFIX,argotunnel.com,DIRECT
  - IP-CIDR,198.41.192.0/24,DIRECT,no-resolve
  - IP-CIDR,198.41.200.0/24,DIRECT,no-resolve
```

第一条覆盖 `region1.v2.argotunnel.com` 和 `region2.v2.argotunnel.com`，**在 fake-ip 模式下同样有效**：Clash 内部记着假 IP 背后对应的域名，所以域名规则照常命中，不需要为此关掉 fake-ip。后两条是按 IP 的冗余兜底。`no-resolve` 是防止 Clash 为了匹配这条规则反而去解析域名。

两个坑：

- **配置来自订阅的话，手工加的规则下次更新订阅就被覆盖。** 要持久化得用客户端的覆写功能（OpenClash 是「覆写设置 → 自定义规则」，Mihomo 系用 override/mixin，或者在订阅转换里注入规则）。直接改落地的 config 只是临时生效。
- **别想着改 DNS 绕过。** TUN 模式下 LAN 的出站流量全部进 TUN 走规则匹配，跟域名解析成什么 IP 无关；改了反而让匹配从「按域名」退化成「按 IP」——`argotunnel.com` 是 Cloudflare 的 IP，很可能被兜底规则判去代理，结果还是走代理，只是变得不可预测。

改完在代理面板的 connections 页面筛 `argotunnel`，看 `chains` 显示 `DIRECT` 就对了。

## 安全模型

- **对外那一面只监听 `127.0.0.1`**。只有同机的 cloudflared 能连到它，所以 `CF-Connecting-IP` 之类的转发头可以放心信任，内网其他机器既伪造不了也看不到登录页。开了 `lan_index` 会多出一个内网监听，它是独立的 handler，碰不到反代，也不信转发头（记日志用的是内核给的地址）。那个监听器上**确实有登录**——管理页的——但它有自己的签名密钥和自己的 Cookie，和门户那套互不通用。隔离靠的是**派生密钥**（`HMAC(sessionKey, "webvpn-lan-admin")`）而不是某处 if 判断，所以是构造上成立的。
- **公网入口强制 HTTPS。** `public_scheme: https` 时，应用会给所有公网响应加 `Strict-Transport-Security: max-age=15552000; includeSubDomains`，并把从 http 进来的请求（包括登录 POST）以 308 跳回 https。判断依据是 Cloudflare 的 `CF-Visitor` / `X-Forwarded-Proto`，不是 `r.TLS`——origin 这一跳永远是明文 http。HSTS 只在浏览器见过一次 https 后才生效，所以 Cloudflare 面板里的 **Always Use HTTPS** 仍应打开，两层互为兜底。`includeSubDomains` 覆盖整个 `example.com`，域内不能再有只能走 http 的子域；另外 Tunnel 在 Cloudflare 边缘终止 TLS，Cloudflare 本身能看到请求内容，这是用 Tunnel 的信任前提。
- **登录 ≠ 有权访问。** 这是加权限之后最要紧的一条：认证只回答「你是谁」，授权每次请求现查一遍「你能不能到这个子域名」。token 里只有用户名、没有权限集合，所以改权限、删账号都是下一次请求就生效，不用等过期也不用重启；一个被删掉的账号手里那个签名仍然合法的 Cookie 立刻什么都打不开。admin 隐含拥有全部。
- **ACL 只覆盖隧道那条路。** 后端自己在局域网里是直接可达的（`192.168.1.50:9191` 这种），同网段的人绕过索引页直接敲地址就行。所以权限的语义是「从公网经隧道进来这条路对你设防」，**不是「这个服务你看不到」**。内网索引页因此照旧免登录列全部。
- 目标地址在 **`Dialer.Control`** 里校验，只允许私有地址段。放在这里是刻意的：它跑在 DNS 解析之后、connect 之前，所以能挡住"域名解析到公网"和 DNS rebinding，而只做配置校验挡不住。
- 会话 Cookie 从转发给后端的请求里剥掉，内网设备拿不到可重放的凭证。
- 登录限流四层：**失败锁定**（同一 IP 在 `login.failure_window` 内错 `login.max_failures` 次就锁 `login.lockout`，重复触发翻倍、`max_lockout` 封顶；状态在服务端内存里，刷新页面不会解除，IPv6 按 /64 计数）→ 按 IP 的令牌桶 → 并发信号量（bcrypt 是 CPU 炸弹）→ 未知用户也跑一次假校验，避免用响应时间枚举用户名。锁定**按 IP 不按用户名**，否则攻击者故意输错就能把真实账号锁死；代价是同一出口 IP 的人共享锁定，且多 IP 分布式爆破仍只能被令牌桶限速。管理页复用同一套，但用**独立的限流器**，键是内核给的 `RemoteAddr`（那个监听器上任何客户端都能伪造转发头）。状态是进程内的，重启即清空。
- 管理页改动的请求都校验 `Origin`（缺失时退回 `Referer`）必须是同一个 host，两者都没有就拒绝；配合 Cookie 的 `SameSite=Lax`。没上 CSRF token，这是有意的取舍，理由写在 `sameOrigin` 的注释里。
- 日志不记录 Cookie、`Set-Cookie`、`Authorization`，也不记录 query string（不少内网后台把 token 放在 query 里）。

**唯一的红线：永远不要加"代理任意 URL"的能力**（比如 `?url=` 参数）。那会让上面整套白名单归零，把进程变成开放的 SSRF 跳板。

## 许可证

MIT，见 [LICENSE](LICENSE)。
