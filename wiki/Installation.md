# Installation

**English** | [简体中文](Installation-zh)

This page takes you from a clone to a running server on your machine. For a public deployment behind Cloudflare, continue with [Deployment](Deployment).

## Requirements

| Component | Version | Needed for |
|---|---|---|
| Go | 1.26+ | Building the server |
| Cloudflare account + domain | — | Public access through a tunnel |
| `cloudflared` | recent | Public access |
| Python | 3.9+ | `push.py` only |

The server itself is a static binary. It has no runtime dependencies beyond the OS and, for public access, `cloudflared`.

## Build

```bash
git clone https://github.com/LCBRST/WebVPN.git
cd WebVPN
go build -o webvpn .
```

The repository is a normal Go module (`github.com/LCBRST/webvpn`). `go test ./...` should pass before you deploy a build.

## Generate secrets

Two values are required in every config: a session signing key and at least one bcrypt password hash.

```bash
./webvpn -genkey
# 8f3c... (32 random bytes, base64; paste into session.signing_key)

./webvpn -hash
# Password: ****
# Repeat:   ****
# $2a$12$...
```

`-hash` reads the password from stdin on purpose. A password in `argv` is visible to every user on the machine through `ps`.

You can also supply the signing key as `WEBVPN_SESSION_SECRET`; it overrides the config value, which keeps it out of backups.

## Create a config

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml
```

A minimal working file:

```yaml
server:
  listen: "127.0.0.1:8080"
  base_domain: "example.com"
  portal_host: "vpn.example.com"
  public_scheme: "https"

session:
  signing_key: "<output of webvpn -genkey>"
  ttl: 168h
  refresh_after: 84h
  epoch: 1

users:
  - name: "admin"
    password_hash: "<output of webvpn -hash>"
    admin: true

services:
  - name: "Router"
    subdomain: "router-vpn"
    target: "http://192.168.1.1"
```

`base_domain` is the zone apex, and `portal_host` must be the apex or a subdomain of it. Service URLs are built as `<subdomain>.<base_domain>`.

For production, add a `state.path` pointing at a writable directory; see [Configuration](Configuration#state).

## Validate before starting

```bash
./webvpn -check -config config.yaml
# config ok: 1 service(s), portal at https://vpn.example.com, state at ...
```

`-check` parses and validates everything without starting listeners and **without writing state**. It is safe to run against a production config over SSH.

Unknown keys are an error rather than a silent no-op:

```
webvpn: config.yaml: services[0]: unknown field "subdomian"
```

## Run

```bash
./webvpn -config config.yaml
```

On first start, if `state.json` does not exist, it is seeded from `config.yaml` and logged. If no user has `admin: true`, the first user is promoted with a warning. From then on `state.json` is authoritative and the `users:` / `services:` sections in the config file are ignored.

## Try it locally without Cloudflare

1. Set `public_scheme: http`.
2. Set `listen: "127.0.0.1:80"` — service links are generated without a port, so port 80 keeps them clickable. (Binding port 80 may need administrator rights; if you use another port, open it explicitly and expect card links to omit it.)
3. Add hosts entries:

```
127.0.0.1 vpn.example.com router-vpn.example.com
```

4. Start the server and open `http://vpn.example.com/`.

Log in with the account from `config.yaml`. The service card should point at `http://router-vpn.example.com/`, and the reverse proxy should reach your router.

For local testing, leave `state.path` unset; it defaults to `state.json` next to the config. Delete that file to re-seed from the config after editing users or services.

## First-run checklist

- [ ] `-check` prints `config ok`
- [ ] the portal loads and accepts your login
- [ ] an unknown password is rejected
- [ ] a service card opens the backend
- [ ] `state.json` was created and contains your users and services
- [ ] logs contain no unused warnings about `insecure_tls` or missing `lan_index.host`

## Next

- [Configuration](Configuration) — every key and validation rule
- [Deployment](Deployment) — systemd, Cloudflare Tunnel, certificates
- [LAN Index and Admin](LAN-Index-and-Admin) — run the admin page on your LAN
