# 安装

[English](Installation) | **简体中文**

这一页带你从 clone 到本机跑起来。要部署到公网 Cloudflare 后面，接着看 [部署](Deployment-zh)。

## 环境要求

| 组件 | 版本 | 用途 |
|---|---|---|
| Go | 1.26+ | 编译服务端 |
| Cloudflare 账号 + 域名 | — | 通过隧道公网访问 |
| `cloudflared` | 较新版本 | 公网访问 |
| Python | 3.9+ | 仅 `push.py` 需要 |

服务端本身是静态二进制，除了操作系统没有运行时依赖；公网访问另需 `cloudflared`。

## 编译

```bash
git clone https://github.com/LCBRST/WebVPN.git
cd WebVPN
go build -o webvpn .
```

仓库是普通 Go module（`github.com/LCBRST/webvpn`）。部署前先跑一遍 `go test ./...`。

## 生成密钥

配置里必须有两样东西：session 签名密钥和至少一个 bcrypt 密码哈希。

```bash
./webvpn -genkey
# 8f3c...（32 字节随机数，base64；粘到 session.signing_key）

./webvpn -hash
# Password: ****
# Repeat:   ****
# $2a$12$...
```

`-hash` 刻意从 stdin 读密码。写在 argv 里的密码，机器上任何用户都能通过 `ps` 看到。

签名密钥也可以用 `WEBVPN_SESSION_SECRET` 提供，它会覆盖配置文件里的值，顺便避免密钥进备份。

## 创建配置

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml
```

一份能跑的最小配置：

```yaml
server:
  listen: "127.0.0.1:8080"
  base_domain: "example.com"
  portal_host: "vpn.example.com"
  public_scheme: "https"

session:
  signing_key: "<webvpn -genkey 的输出>"
  ttl: 168h
  refresh_after: 84h
  epoch: 1

users:
  - name: "admin"
    password_hash: "<webvpn -hash 的输出>"
    admin: true

services:
  - name: "路由器"
    subdomain: "router-vpn"
    target: "http://192.168.1.1"
```

`base_domain` 是域名的 zone apex，`portal_host` 必须等于它或是它的子域。服务地址按 `<subdomain>.<base_domain>` 生成。

生产环境还要加 `state.path` 指到一个可写目录，见 [配置](Configuration-zh#state)。

## 启动前校验

```bash
./webvpn -check -config config.yaml
# config ok: 1 service(s), portal at https://vpn.example.com, state at ...
```

`-check` 只解析和校验，不启动监听，**也不写状态**。可以放心通过 SSH 对生产配置跑。

未知键会直接报错，而不是静默忽略：

```
webvpn: config.yaml: services[0]: unknown field "subdomian"
```

## 启动

```bash
./webvpn -config config.yaml
```

首次启动时如果 `state.json` 不存在，会从 `config.yaml` 播种并打日志。如果没有任何用户带 `admin: true`，第一个用户会被提升为管理员并打一条警告。从那以后 `state.json` 是权威，配置文件里的 `users:` / `services:` 不再生效。

## 不碰 Cloudflare 的本地试跑

1. 把 `public_scheme` 设为 `http`。
2. 把 `listen` 设为 `"127.0.0.1:80"`——生成的卡片链接不带端口，用 80 才能直接点开。（绑 80 可能需要管理员权限；用别的端口也行，但访问时要显式带端口，并且卡片链接会缺端口。）
3. 加 hosts：

```
127.0.0.1 vpn.example.com router-vpn.example.com
```

4. 启动后打开 `http://vpn.example.com/`。

用 `config.yaml` 里的账号登录。服务卡片应该指向 `http://router-vpn.example.com/`，反向代理应该能打到你的路由器。

本地测试可以不配 `state.path`，默认落在配置文件旁边的 `state.json`。改完 users/services 想重新播种，删掉这个文件即可。

## 首次运行检查清单

- [ ] `-check` 输出 `config ok`
- [ ] 门户能打开，登录成功
- [ ] 错误密码被拒绝
- [ ] 服务卡片能打开后端
- [ ] `state.json` 已生成，里面有用户和服务
- [ ] 日志里没有 `insecure_tls` 或缺少 `lan_index.host` 的告警

## 下一步

- [配置](Configuration-zh) —— 所有键和校验规则
- [部署](Deployment-zh) —— systemd、Cloudflare Tunnel、证书
- [内网索引与管理页](LAN-Index-and-Admin-zh) —— 在内网跑管理页
