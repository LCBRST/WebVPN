# WebVPN

[![CI](https://img.shields.io/github/actions/workflow/status/LCBRST/WebVPN/ci.yml?branch=main&label=CI)](https://github.com/LCBRST/WebVPN/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/LCBRST/WebVPN)](go.mod)
[![License](https://img.shields.io/github/license/LCBRST/WebVPN)](LICENSE)

[English](README.md) | **简体中文**

在浏览器里打开一个网址，就能访问内网的 Web 服务（路由器后台、NAS 面板、Grafana……）。通过 Cloudflare Tunnel 暴露成各自的 HTTPS 子域名，统一登录。不需要装客户端，不需要在每台设备上配代理，不需要公网 IP。

```
浏览器 ──HTTPS──> Cloudflare 边缘 ──隧道──> cloudflared ──HTTP──> webvpn ──> 内网服务
       (外网)                   (TLS 在此终止)  (内网机器)   (127.0.0.1:8080)  (192.168.x.x)
```

## 特性

- **一个服务一个子域名。** `nas-vpn.example.com` 一对一映射到 `http://192.168.1.10:5000`。路径完全不变，所以 SPA、WebSocket 和写死的静态资源路径都能直接工作，不需要改写 HTML/CSS/JS。
- **一次登录，全站通行。** 无状态 HMAC 签名 Cookie 覆盖整个 base domain，重启不掉线。要全员下线就把 `session.epoch` 加 1。
- **按用户授权。** 默认拒绝，每次请求现查权限。收回某个服务的访问、或者直接删掉账号，下一次请求就生效——不用重启，也不用等 Cookie 过期。
- **内网管理页。** 在浏览器里加服务、管用户、勾权限，改完立即生效；管理页从不回写 `config.yaml`。
- **内网索引页。** `http://<内网IP>/` 是一张免登录的卡片列表，人在屋里时直接点，不用绕 Cloudflare。
- **默认安全。** 回源监听只绑回环，后端必须是私有地址；登录有 bcrypt、限流和渐进式锁定；会话 Cookie 在转发前被剥掉；日志里没有令牌。
- **单个静态二进制。** 纯 Go，无运行时依赖，只需要配套一个 `cloudflared`。

## 工作原理

1. `cloudflared` 主动向外连接 Cloudflare，把流量转发给本机 `127.0.0.1` 上的 webvpn。
2. webvpn 按 `Host` 路由：门户域名提供登录和服务索引，其余 `*.<base_domain>` 经鉴权后反向代理到对应后端。
3. 运行时可变的状态——服务、用户、访问规则——存在 `state.json`；其余配置留在 `config.yaml`，只在启动时读一次。

两个决定全局形态的取舍：

- **子域名路由，不是路径前缀。** `/proxy/192.168.1.10/` 这种形式会让绝对 URL、`Location` 跳转和 Cookie 路径全部指错，修它就得改写响应体。子域名模式让路径保持 1:1，代价是需要泛域名 DNS 和泛域名证书。
- **无状态会话。** 重启代理不会把人踢下线。代价是单个会话无法吊销；但权限和账号是每次请求现查的，`session.epoch` 则是"全部吊销"的逃生门。

## 快速开始

要求 Go 1.26 或更新版本。

```bash
git clone https://github.com/LCBRST/WebVPN.git
cd WebVPN

go build -o webvpn .

./webvpn -genkey                  # 生成 session 签名密钥
printf 'yourpassword\nyourpassword\n' | ./webvpn -hash   # 生成 bcrypt 哈希，别走 argv

cp config.example.yaml config.yaml
$EDITOR config.yaml               # 填 signing_key、password_hash、base_domain、services

./webvpn -check -config config.yaml
./webvpn -config config.yaml
```

`-check` 只校验配置，不启动服务、不写状态。未知键会直接报错，打错一个字母不会退化成"改了没反应"。

### 本地试跑

把 `public_scheme` 设为 `http`、`listen` 设为 `"127.0.0.1:80"`，再加 hosts：

```
127.0.0.1 vpn.example.com nas-vpn.example.com
```

然后打开 `http://vpn.example.com/`。用 80 端口是因为生成的卡片链接不带端口；如果监听别的端口，访问时要手动带上，并且卡片链接会缺端口。

## 配置

配置分两半，这是最容易搞混的地方：


| 改什么                                                                | 在哪改                                | 生效方式 |
| --------------------------------------------------------------------- | ------------------------------------- | -------- |
| 服务、用户、谁能访问哪些站点                                          | 内网管理页（`http://<内网IP>/admin`） | 立即     |
| 其余全部（`server`、`session`、`login`、`lan_index`、每个服务的开关） | `config.yaml`                         | 重启后   |

`config.yaml` 里的 `users:` 和 `services:` 只在 `state.json` 第一次生成时作为**种子**。文件一旦存在，一切以它为准：在管理页删掉的用户不会因重启复活，往配置里加用户也不会生效。

加一个服务最少三行：

```yaml
services:
  - name: "Ollama"
    subdomain: "ollama-vpn"
    target: "http://192.168.1.50:11434"
```

完整的键说明、校验规则和每个服务的开关见 **[配置文档](https://github.com/LCBRST/WebVPN/wiki/Configuration-zh)**。

## 部署

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o webvpn-linux-amd64 .
```

把二进制和 `config.yaml` 拷到服务器，建一个 `webvpn` 系统用户，装上 `deploy/webvpn.service`。然后用 Cloudflare Tunnel 指向 `127.0.0.1:8080`，示例见 `deploy/cloudflared.example.yml`。

`push.py` 把整个流程自动化（编译 → 备份 → 上传 → 用服务器现有配置跑 `-check` → 替换二进制和单元 → 重启 → 验证），任何一步失败都会自动回滚。它需要 Python 3 和 `pip install -r requirements.txt`，凭据从 `creds.json` 或 `WEBVPN_CREDS` 读取。

完整步骤、证书层级限制和 Cloudflare 后台设置见 **[部署文档](https://github.com/LCBRST/WebVPN/wiki/Deployment-zh)**。

## 文档


| 主题                           | English                                                                          | 中文                                                                             |
| ------------------------------ | -------------------------------------------------------------------------------- | -------------------------------------------------------------------------------- |
| 安装与首次运行                 | [Installation](https://github.com/LCBRST/WebVPN/wiki/Installation)               | [安装](https://github.com/LCBRST/WebVPN/wiki/Installation-zh)                    |
| 全部配置项                     | [Configuration](https://github.com/LCBRST/WebVPN/wiki/Configuration)             | [配置](https://github.com/LCBRST/WebVPN/wiki/Configuration-zh)                   |
| 服务器、隧道、systemd、push.py | [Deployment](https://github.com/LCBRST/WebVPN/wiki/Deployment)                   | [部署](https://github.com/LCBRST/WebVPN/wiki/Deployment-zh)                      |
| 内网索引与管理页               | [LAN Index and Admin](https://github.com/LCBRST/WebVPN/wiki/LAN-Index-and-Admin) | [内网索引与管理页](https://github.com/LCBRST/WebVPN/wiki/LAN-Index-and-Admin-zh) |
| 改写、WebSocket、SSE           | [Proxy Behavior](https://github.com/LCBRST/WebVPN/wiki/Proxy-Behavior)           | [代理行为](https://github.com/LCBRST/WebVPN/wiki/Proxy-Behavior-zh)              |
| 威胁模型与局限                 | [Security](https://github.com/LCBRST/WebVPN/wiki/Security)                       | [安全模型](https://github.com/LCBRST/WebVPN/wiki/Security-zh)                    |
| 症状 → 原因对照表             | [Troubleshooting](https://github.com/LCBRST/WebVPN/wiki/Troubleshooting)         | [排查](https://github.com/LCBRST/WebVPN/wiki/Troubleshooting-zh)                 |

## 安全要点

- 公网监听只绑 `127.0.0.1`，只有本机 `cloudflared` 能连到它，所以来自它的转发头可以信任。
- 后端目标在 DNS 解析之后校验是否为私有地址，同时也挡住 DNS rebinding；代码拒绝变成开放中继。
- 登录限制：按 IP 的令牌桶、同时只跑一次 bcrypt、渐进式失败锁定、未知用户也跑一次假校验，避免用时间枚举账号。
- 会话 token 只带用户名、不带权限；权限每次请求从 store 现查。
- Cookie、`Set-Cookie`、`Authorization` 和 query string 从不进日志。

在暴露敏感服务之前，请读完完整的 **[安全模型](https://github.com/LCBRST/WebVPN/wiki/Security-zh)**。唯一的红线：永远不要加"代理任意 URL"的能力，那会把白名单变成开放的 SSRF 跳板。

## 项目结构

```
main.go                  入口：-check、-hash、-genkey
internal/config/         YAML 解析与校验
internal/store/          state.json：用户、服务、访问规则
internal/auth/           登录、签名 Cookie、锁定与限流
internal/proxy/          反向代理，Location/Cookie/CORS/Origin 改写
internal/portal/         门户与管理页模板
internal/app/            路由、监听器、中间件
deploy/                  systemd 单元与 cloudflared 示例
push.py                  一键部署脚本（依赖见 requirements.txt）
wiki/                    GitHub wiki 源文件，中英双语
```

## 开发

```bash
gofmt -l .        # 必须没有输出
go vet ./...
go test ./...
```

CI 会跑同样的检查，外加一次 `linux/amd64` 构建。依赖刻意保持很少：只有 `golang.org/x/crypto` 和 `gopkg.in/yaml.v3`。

## 贡献

欢迎提 issue 和 pull request。这个项目本身是安全边界，较大的行为改动请先开 issue 讨论；修 bug 请带上测试。永远不要提交 `config.yaml`、`state.json`、`creds.json`，或任何包含真实域名、地址、密码哈希、签名密钥的内容。

## 许可证

[MIT](LICENSE)
