# 部署

[English](Deployment) | **简体中文**

这一页讲生产安装：Linux 主机、systemd 单元、Cloudflare Tunnel，以及可选的 `push.py`。

```
公网 ──HTTPS──> Cloudflare 边缘 ──隧道──> cloudflared ──HTTP──> webvpn ──> 内网服务
                                    (同机)                  127.0.0.1:8080
```

## 环境要求

- 一台能访问后端的 Linux 内网主机。
- DNS 托管在 Cloudflare 的域名。
- 已安装并能连上 Cloudflare 的 `cloudflared`。
- 构建机需要 Go 1.26+（服务器本身不需要）。
- 启用 `lan_index` 时，内网地址上的 80 端口要空闲（也可以用高位端口）。

## 编译

在工作机上交叉编译：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o webvpn-linux-amd64 .
```

ARM 服务器把 `GOARCH` 换成 `arm64`。二进制是静态的，没有 CGO 依赖。

## 安装服务

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin webvpn
sudo install -d -o webvpn -g webvpn -m 0755 /opt/webvpn
sudo install -o webvpn -g webvpn -m 0755 webvpn-linux-amd64 /opt/webvpn/webvpn
sudo install -o webvpn -g webvpn -m 0600 config.yaml /opt/webvpn/config.yaml
sudo install -m 0644 deploy/webvpn.service /etc/systemd/system/webvpn.service
sudo systemctl daemon-reload
sudo systemctl enable --now webvpn
```

单元从 `/opt/webvpn/config.yaml` 读配置，以 `webvpn` 用户运行，可变状态存在 `/var/lib/webvpn/state.json`。

### systemd 单元

`deploy/webvpn.service` 的权限收得很紧，关键几行：

```ini
User=webvpn
Group=webvpn

AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true

StateDirectory=webvpn
StateDirectoryMode=0700
```

- **`StateDirectory=webvpn`** 创建 `/var/lib/webvpn`、把属主设为 `webvpn`，并在 `ProtectSystem=strict` 下让它可写。所以 `state.path` 必须写 `/var/lib/webvpn/state.json`；用默认值（配置文件旁边）会落到只读目录，启动时失败，而且报错看着像权限问题。
- **Capability** 只在 `lan_index.listen` 使用 1024 以下端口时需要。`AmbientCapabilities` 和 `CapabilityBoundingSet` 两行缺一不可。
- **`NoNewPrivileges=true` 会让 `setcap` 静默失效。** 文件 capability 被内核忽略，必须从单元里给。一个"设过 setcap"却仍然 `bind: permission denied` 的二进制，就是这个坑。

看状态和日志：

```bash
systemctl status webvpn
journalctl -u webvpn -f
ss -lntp | grep webvpn
```

## Cloudflare Tunnel

隧道有两种管理方式，选一种就别混用；dashboard 管理和本地文件管理混着改，是"改了没生效"的常见原因。

### Dashboard 管理

在 Cloudflare Zero Trust 面板创建隧道、安装 connector，然后在界面上加 public hostname：

| Public hostname | Service |
|---|---|
| `vpn.example.com` | `http://127.0.0.1:8080` |
| `*-vpn.example.com` | `http://127.0.0.1:8080` |

通配规则不匹配 apex，所以 `vpn.example.com` 需要单独一条记录和 ingress。

### 本地文件管理

```bash
cloudflared tunnel login
cloudflared tunnel create webvpn
cloudflared tunnel route dns webvpn "vpn.example.com"
cloudflared tunnel route dns webvpn "*-vpn.example.com"
cloudflared tunnel ingress validate
cloudflared tunnel ingress rule https://router-vpn.example.com
sudo cloudflared service install
```

ingress 文件见 `deploy/cloudflared.example.yml`，替换 `<tunnel-id>` 和域名。最后必须有兜底规则：

```yaml
  - service: http_status:404
```

## DNS 与证书

**服务名必须恰好是 base domain 下的一层。** Cloudflare 免费版 Universal SSL 覆盖 `example.com` 和 `*.example.com`，也就是一层：

- `router-vpn.example.com` ✅ 有证书
- `router.vpn.example.com` ❌ 边缘没有证书，握手报 `ERR_SSL_VERSION_OR_CIPHER_MISMATCH`

cloudflared 的通配 ingress 照样会路由两层的名字，所以"隧道通了"不能说明证书没问题。要用浏览器打开看证书链。

两层名字需要 Advanced Certificate Manager（付费，自动签 `*.vpn.example.com`）或上传自定义泛域名证书。另一个办法是把 `base_domain` 改成 `vpn.example.com`，调整 subdomain 让所有服务都落在它下面一层。

## 必须检查的 Cloudflare 设置

这些都不在 webvpn 的配置里，漏了会出怪问题：

| 设置 | 值 | 原因 |
|---|---|---|
| 这些域名的 Cache Rule | **Bypass cache** | 否则边缘可能把某个会话的页面缓存给另一个客户端。 |
| Rocket Loader | **关** | 延迟执行内联脚本，老设备面板表现为按钮点了没反应。 |
| Auto Minify | **关** | 改写 HTML/JS，破坏对空白敏感的模板。 |
| Email Obfuscation | **关** | 把地址替换成脚本，可能弄坏管理页。 |
| Always Use HTTPS | **开** | webvpn 也会重定向并发 HSTS，但要浏览器先见过一次 HTTPS 才生效。 |

webvpn 还会对 HTML 响应强制 `Cache-Control: private, no-store`，作为第二道保险。

## 用 `push.py` 部署

`push.py` 是可选的，它把整个流程自动化，并且专门设计成对线上服务器安全。

依赖：

```bash
pip install -r requirements.txt
```

凭据查找顺序：

1. `$WEBVPN_CREDS`
2. `./creds.json`（已被 git 忽略）
3. `~/.config/webvpn/creds.json`

文件格式：

```json
{
  "host": "192.168.1.50",
  "port": 22,
  "user": "root",
  "password": "..."
}
```

然后：

```bash
python push.py
```

它做的事：

1. 交叉编译 `linux/amd64`。
2. 带时间戳备份服务器上的二进制、单元和配置。
3. 上传新二进制为 `.new`，用**服务器上现有的配置**跑 `-check`。新二进制不认旧配置就在这里停下，机器上什么都没动。
4. 远程配置缺少 `lan_index` / `state` 段时追加，**只追加，不覆盖**。
5. 上传单元、原子替换二进制、daemon-reload 并重启。
6. 验证服务 active、80 绑在内网地址而不是 `0.0.0.0`、索引页返回 200 且有卡片、`/admin` 返回 302、`state.json` 是 `webvpn:webvpn 600`。

任何一步失败都会回滚到带时间戳的备份，并打印回滚点。

它刻意**不上传本地的 `config.yaml`**（服务器那份才有真实签名密钥和密码哈希，服务清单也可能更全），也**不碰 `state.json`**。

## 运维

升级前备份两个文件：

```bash
sudo cp /opt/webvpn/config.yaml /opt/webvpn/config.yaml.bak-$(date +%F)
sudo cp /var/lib/webvpn/state.json /var/lib/webvpn/state.json.bak-$(date +%F)
```

升级：

```bash
sudo install -o webvpn -g webvpn -m 0755 webvpn-linux-amd64 /opt/webvpn/webvpn.new
sudo -u webvpn /opt/webvpn/webvpn.new -check -config /opt/webvpn/config.yaml
sudo mv /opt/webvpn/webvpn.new /opt/webvpn/webvpn
sudo systemctl restart webvpn
```

如果 `-check` 不认现有状态（比如将来 schema 版本升级），不要直接覆盖；读报错、留好备份、看发布说明。

### 轮换密钥

- **签名密钥：** 生成新的，替换 `session.signing_key`，重启。所有人重新登录。
- **只让所有人下线：** `session.epoch` 加 1，重启。
- **改密码：** 在管理页改，下次登录生效。已签发的会话在过期或 `epoch` 加 1 之前仍然有效。

## 下一步

- [内网索引与管理页](LAN-Index-and-Admin-zh) —— 第二个监听器和管理界面
- [安全模型](Security-zh) —— 这套部署保护了什么、没保护什么
- [排查](Troubleshooting-zh) —— 服务起来了但不好用时看这里
