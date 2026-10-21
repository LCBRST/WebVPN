# webvpn Wiki

**English** | [简体中文](Home#中文)

webvpn exposes LAN web services on their own HTTPS subdomains through a Cloudflare Tunnel, behind one login. The main [README](https://github.com/LCBRST/WebVPN/blob/main/README.md) is the short version; this wiki is the reference.

```
Browser ──HTTPS──> Cloudflare edge ──tunnel──> cloudflared ──HTTP──> webvpn ──> LAN service
        (internet)                  (TLS ends here)  (your host)   (127.0.0.1:8080)  (192.168.x.x)
```

## Documentation

| Topic | English | 中文 |
|---|---|---|
| Installation and first run | [Installation](Installation) | [安装](Installation-zh) |
| Every configuration key | [Configuration](Configuration) | [配置](Configuration-zh) |
| Server, tunnel, systemd, push.py | [Deployment](Deployment) | [部署](Deployment-zh) |
| LAN index and admin page | [LAN Index and Admin](LAN-Index-and-Admin) | [内网索引与管理页](LAN-Index-and-Admin-zh) |
| Rewriting, WebSockets, SSE | [Proxy Behavior](Proxy-Behavior) | [代理行为](Proxy-Behavior-zh) |
| Threat model and limitations | [Security](Security) | [安全模型](Security-zh) |
| Symptom → cause table | [Troubleshooting](Troubleshooting) | [排查](Troubleshooting-zh) |

## What it is

- A reverse proxy that maps `service.<base_domain>` 1:1 to an internal URL. Paths are never rewritten, so SPAs and WebSockets work.
- A login gate with stateless signed cookies and per-user, per-service access rules.
- A LAN-only index and admin page for managing services and users without editing YAML.

## What it is not

- Not a general-purpose proxy: targets must be private addresses, and there is deliberately no "proxy this URL" parameter.
- Not a replacement for Cloudflare Access. It is a single-user-ish self-hosted gate for a home or small-office network.
- Not a VPN in the tunnel sense. It proxies HTTP(S) only; protocols such as SSH or RTMP need something else.

## Requirements

Go 1.26+ to build. A Cloudflare-managed domain and `cloudflared` for public access. Python 3 only if you use `push.py`.

Start with [Installation](Installation).

---

## 中文

webvpn 通过 Cloudflare Tunnel 把内网 Web 服务暴露成各自的 HTTPS 子域名，统一登录。主 [README](https://github.com/LCBRST/WebVPN/blob/main/README.zh-CN.md) 是简版，这里放完整参考。

## 文档

| 主题 | English | 中文 |
|---|---|---|
| 安装与首次运行 | [Installation](Installation) | [安装](Installation-zh) |
| 全部配置项 | [Configuration](Configuration) | [配置](Configuration-zh) |
| 服务器、隧道、systemd、push.py | [Deployment](Deployment) | [部署](Deployment-zh) |
| 内网索引与管理页 | [LAN Index and Admin](LAN-Index-and-Admin) | [内网索引与管理页](LAN-Index-and-Admin-zh) |
| 改写、WebSocket、SSE | [Proxy Behavior](Proxy-Behavior) | [代理行为](Proxy-Behavior-zh) |
| 威胁模型与局限 | [Security](Security) | [安全模型](Security-zh) |
| 症状 → 原因对照表 | [Troubleshooting](Troubleshooting) | [排查](Troubleshooting-zh) |

## 它是什么

- 一个反向代理，把 `service.<base_domain>` 一对一映射到内网地址，路径从不改写，SPA 和 WebSocket 可以直接用。
- 一道登录门，用无状态签名 Cookie，加上按用户、按服务的访问规则。
- 一个只在内网开放的服务索引和管理页，不用改 YAML 就能管服务和用户。

## 它不是什么

- 不是通用代理：目标必须是私有地址，而且刻意没有"代理任意 URL"的参数。
- 不是 Cloudflare Access 的替代品。它是面向家庭或小型办公网络的自托管入口。
- 不是隧道意义上的 VPN。它只代理 HTTP(S)，SSH、RTMP 这类协议要另想办法。

## 环境要求

构建需要 Go 1.26+。公网访问需要一个托管在 Cloudflare 的域名和 `cloudflared`。只有用 `push.py` 才需要 Python 3。

从 [安装](Installation-zh) 开始。
