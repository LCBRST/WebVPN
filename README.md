# WebVPN

[![CI](https://img.shields.io/github/actions/workflow/status/LCBRST/WebVPN/ci.yml?branch=main&label=CI)](https://github.com/LCBRST/WebVPN/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/LCBRST/WebVPN)](go.mod)
[![License](https://img.shields.io/github/license/LCBRST/WebVPN)](LICENSE)

**English** | [简体中文](README.zh-CN.md)

Expose your LAN web apps — router admin panels, NAS, Grafana, anything HTTP — on their own HTTPS subdomains through a Cloudflare Tunnel, behind a single login. No client apps, no per-device proxy configuration, no public IP.

```
Browser ──HTTPS──> Cloudflare edge ──tunnel──> cloudflared ──HTTP──> webvpn ──> LAN service
        (internet)                  (TLS ends here)  (your host)   (127.0.0.1:8080)  (192.168.x.x)
```

## Features

- **One subdomain per service.** `nas-vpn.example.com` maps 1:1 to `http://192.168.1.10:5000`. Because paths are untouched, SPAs, WebSockets and absolute asset paths keep working without HTML/CSS/JS rewriting.
- **One login for everything.** A stateless, HMAC-signed session cookie is shared across the base domain and survives restarts. Bump `session.epoch` to invalidate every session at once.
- **Per-user access control.** Authorization is default-deny and checked on every request, so revoking a grant or deleting an account takes effect on the next request — no restart, no waiting for a cookie to expire.
- **Admin page on the LAN.** Add services, manage users and toggle access from a browser. Changes apply immediately; the admin page never rewrites `config.yaml`.
- **LAN index.** `http://<lan-ip>/` serves an unauthenticated card list linking straight to each service, for when you are already inside the network.
- **Secure by default.** The origin listener is loopback-only, upstreams must be private addresses, login has bcrypt + rate limiting + progressive lockout, session cookies are stripped before proxying, and tokens never reach the logs.
- **Single static binary.** Pure Go, no runtime dependencies. `cloudflared` is the only companion process.

## How it works

1. `cloudflared` maintains an outbound tunnel to Cloudflare and forwards traffic to webvpn on `127.0.0.1`.
2. webvpn routes by `Host`: the portal host serves login and the service index, and every other `*.<base_domain>` host is authorized and reverse-proxied to its backend.
3. State that changes at runtime — services, users, access rules — lives in `state.json`. Everything else stays in `config.yaml` and is read once at startup.

The two design choices that shape the rest of the project:

- **Subdomains, not path prefixes.** A path prefix (`/proxy/192.168.1.10/`) breaks absolute URLs, `Location` redirects and cookie paths, and fixing that means rewriting response bodies. Subdomain routing keeps the path 1:1, at the cost of wildcard DNS and a wildcard certificate.
- **Stateless sessions.** Restarting the proxy does not log anyone out. Individual sessions cannot be revoked, but grants and accounts are re-checked per request, and `session.epoch` is the escape hatch for revoking everything.

## Quick start

Requirements: Go 1.26 or newer.

```bash
git clone https://github.com/LCBRST/WebVPN.git
cd WebVPN

go build -o webvpn .

./webvpn -genkey                  # session signing key
printf 'yourpassword\nyourpassword\n' | ./webvpn -hash   # bcrypt hash; never pass it in argv

cp config.example.yaml config.yaml
$EDITOR config.yaml               # fill in signing_key, password_hash, base_domain, services

./webvpn -check -config config.yaml
./webvpn -config config.yaml
```

`-check` validates the config without starting the server or writing state. Unknown keys are a hard error, so a typo cannot silently do nothing.

### Try it locally

Set `public_scheme: http`, `listen: "127.0.0.1:80"` and add hosts entries:

```
127.0.0.1 vpn.example.com nas-vpn.example.com
```

Then open `http://vpn.example.com/`. Port 80 matters because generated service links do not include a port; if you listen on another port, use it in the URL and expect card links to omit it.

## Configuration

There are two halves, and the distinction is the thing people trip over:


| What                                                                           | Where                                    | Applies       |
| ------------------------------------------------------------------------------ | ---------------------------------------- | ------------- |
| Services, users, who may reach what                                            | LAN admin page (`http://<lan-ip>/admin`) | Immediately   |
| Everything else (`server`, `session`, `login`, `lan_index`, per-service flags) | `config.yaml`                            | After restart |

`config.yaml`'s `users:` and `services:` only **seed** `state.json` the first time it is created. Once `state.json` exists it is authoritative: deleting a user there is permanent across restarts, and adding one to the config file has no effect.

A minimal service entry needs three lines:

```yaml
services:
  - name: "Ollama"
    subdomain: "ollama-vpn"
    target: "http://192.168.1.50:11434"
```

The full key reference, validation rules and per-service options are in **[Configuration](https://github.com/LCBRST/WebVPN/wiki/Configuration)**.

## Deployment

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o webvpn-linux-amd64 .
```

Copy the binary and `config.yaml` to the server, create a `webvpn` system user, and install `deploy/webvpn.service`. Then point a Cloudflare Tunnel at `127.0.0.1:8080` — see `deploy/cloudflared.example.yml`.

`push.py` automates the whole cycle (build → backup → upload → `-check` against the server's existing config → replace binary and unit → restart → verify), with automatic rollback on any failure. It needs Python 3 and `pip install -r requirements.txt`; credentials are read from `creds.json` or `WEBVPN_CREDS`.

Full instructions, the certificate-depth constraint and Cloudflare-side settings are in **[Deployment](https://github.com/LCBRST/WebVPN/wiki/Deployment)**.

## Documentation


| Topic                            | English                                                                          | 中文                                                                             |
| -------------------------------- | -------------------------------------------------------------------------------- | -------------------------------------------------------------------------------- |
| Installation and first run       | [Installation](https://github.com/LCBRST/WebVPN/wiki/Installation)               | [安装](https://github.com/LCBRST/WebVPN/wiki/Installation-zh)                    |
| Every configuration key          | [Configuration](https://github.com/LCBRST/WebVPN/wiki/Configuration)             | [配置](https://github.com/LCBRST/WebVPN/wiki/Configuration-zh)                   |
| Server, tunnel, systemd, push.py | [Deployment](https://github.com/LCBRST/WebVPN/wiki/Deployment)                   | [部署](https://github.com/LCBRST/WebVPN/wiki/Deployment-zh)                      |
| LAN index and admin page         | [LAN Index and Admin](https://github.com/LCBRST/WebVPN/wiki/LAN-Index-and-Admin) | [内网索引与管理页](https://github.com/LCBRST/WebVPN/wiki/LAN-Index-and-Admin-zh) |
| Rewriting, WebSockets, SSE       | [Proxy Behavior](https://github.com/LCBRST/WebVPN/wiki/Proxy-Behavior)           | [代理行为](https://github.com/LCBRST/WebVPN/wiki/Proxy-Behavior-zh)              |
| Threat model and limitations     | [Security](https://github.com/LCBRST/WebVPN/wiki/Security)                       | [安全模型](https://github.com/LCBRST/WebVPN/wiki/Security-zh)                    |
| Symptom → cause table           | [Troubleshooting](https://github.com/LCBRST/WebVPN/wiki/Troubleshooting)         | [排查](https://github.com/LCBRST/WebVPN/wiki/Troubleshooting-zh)                 |

## Security highlights

- The public listener binds `127.0.0.1`; only the local `cloudflared` can reach it, which is why forwarding headers from it can be trusted.
- Upstream targets are validated as private addresses after DNS resolution, which also defeats DNS rebinding. The code refuses to become an open relay.
- Login limits: per-IP token bucket, a single bcrypt comparison at a time, progressive failure lockout, and a dummy hash for unknown users so timing does not enumerate accounts.
- Session tokens carry a user name but no permissions; grants are read from the store on every request.
- Cookies, `Set-Cookie`, `Authorization` and query strings are never logged.

Read the full **[security model](https://github.com/LCBRST/WebVPN/wiki/Security)** before exposing anything sensitive. The one hard rule: never add a "proxy an arbitrary URL" feature — it would turn the allowlist into an open SSRF relay.

## Project layout

```
main.go                  entry point: -check, -hash, -genkey
internal/config/         YAML parsing and validation
internal/store/          state.json: users, services, access rules
internal/auth/           login, signed cookies, lockout and rate limiting
internal/proxy/          reverse proxy, Location/Cookie/CORS/Origin rewriting
internal/portal/         portal and admin templates
internal/app/            routing, listeners, middleware
deploy/                  systemd unit and cloudflared example
push.py                  one-command deployment (see requirements.txt)
wiki/                    source for the GitHub wiki, in English and Chinese
```

## Development

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

CI runs the same checks plus a `linux/amd64` build. The code is intentionally dependency-light: `golang.org/x/crypto` and `gopkg.in/yaml.v3`.

## Contributing

Issues and pull requests are welcome. Because this is a security boundary, please open an issue to discuss behavior changes before a large pull request, and include a test for anything bug-like. Never commit `config.yaml`, `state.json`, `creds.json`, or anything containing a real domain, address, password hash or signing key.

## License

[MIT](LICENSE)
