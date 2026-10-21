# Deployment

**English** | [简体中文](Deployment-zh)

This page covers a production install: a Linux host, a systemd unit, a Cloudflare Tunnel, and the optional `push.py` helper.

```
Internet ──HTTPS──> Cloudflare edge ──tunnel──> cloudflared ──HTTP──> webvpn ──> LAN services
                                      (same host)              127.0.0.1:8080
```

## Prerequisites

- A Linux host on the LAN that can reach the backends.
- A domain whose DNS is managed by Cloudflare.
- `cloudflared` installed and able to reach Cloudflare.
- Go 1.26+ on the build machine (not necessarily the server).
- Port 80 free on the LAN address if you enable `lan_index` (or choose a high port).

## Build

Cross-compile on your workstation:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o webvpn-linux-amd64 .
```

For ARM servers, use `GOARCH=arm64`. The binary is static; it has no CGO dependencies.

## Install the server

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin webvpn
sudo install -d -o webvpn -g webvpn -m 0755 /opt/webvpn
sudo install -o webvpn -g webvpn -m 0755 webvpn-linux-amd64 /opt/webvpn/webvpn
sudo install -o webvpn -g webvpn -m 0600 config.yaml /opt/webvpn/config.yaml
sudo install -m 0644 deploy/webvpn.service /etc/systemd/system/webvpn.service
sudo systemctl daemon-reload
sudo systemctl enable --now webvpn
```

The unit reads `/opt/webvpn/config.yaml`, runs as `webvpn`, and stores mutable state in `/var/lib/webvpn/state.json`.

### systemd unit

`deploy/webvpn.service` is deliberately strict. The parts that matter:

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

- **`StateDirectory=webvpn`** creates `/var/lib/webvpn`, owns it to `webvpn`, and makes it writable despite `ProtectSystem=strict`. This is why `state.path` must be `/var/lib/webvpn/state.json`; the default (next to the config) would be read-only and the service would fail at startup with what looks like a permissions bug.
- **Capabilities** are only needed if `lan_index.listen` uses a port below 1024. Both `AmbientCapabilities` and `CapabilityBoundingSet` are required.
- **`NoNewPrivileges=true` makes `setcap` silently ineffective.** File capabilities are ignored, so grant the capability from the unit instead. A binary that "has setcap" but still fails with `bind: permission denied` is this exact trap.

Check status and logs:

```bash
systemctl status webvpn
journalctl -u webvpn -f
ss -lntp | grep webvpn
```

## Cloudflare Tunnel

There are two ways to run the tunnel. Pick one and stick to it; mixing dashboard-managed and file-managed configuration is a common source of "the change did nothing".

### Dashboard-managed

Create a tunnel in the Cloudflare Zero Trust dashboard, install its connector on the host, and add public hostnames in the UI:

| Public hostname | Service |
|---|---|
| `vpn.example.com` | `http://127.0.0.1:8080` |
| `*-vpn.example.com` | `http://127.0.0.1:8080` |

A wildcard rule does not match the apex, which is why `vpn.example.com` needs its own entry and DNS record.

### Locally-managed

```bash
cloudflared tunnel login
cloudflared tunnel create webvpn
cloudflared tunnel route dns webvpn "vpn.example.com"
cloudflared tunnel route dns webvpn "*-vpn.example.com"
cloudflared tunnel ingress validate
cloudflared tunnel ingress rule https://router-vpn.example.com
sudo cloudflared service install
```

The ingress file is `deploy/cloudflared.example.yml`. Replace `<tunnel-id>` and the domain. The required catch-all is:

```yaml
  - service: http_status:404
```

## DNS and certificates

**Service names must be exactly one label under the base domain.** Cloudflare's free Universal SSL covers `example.com` and `*.example.com` — one label — so:

- `router-vpn.example.com` ✅ covered
- `router.vpn.example.com` ❌ no edge certificate, handshake fails with `ERR_SSL_VERSION_OR_CIPHER_MISMATCH`

cloudflared's wildcard ingress will happily route a two-label name, so "the tunnel works" is not a certificate test. Open it in a browser and inspect the certificate.

For two-label names you need Advanced Certificate Manager (paid, signs `*.vpn.example.com`) or a custom wildcard certificate. Alternatively set `base_domain: vpn.example.com` and adjust subdomains so everything is one label under it.

## Cloudflare settings that matter

None of these live in webvpn's config, and missing them produces confusing symptoms:

| Setting | Value | Why |
|---|---|---|
| Cache Rule for these hostnames | **Bypass cache** | Otherwise the edge can serve one session's page to another client. |
| Rocket Loader | **Off** | It defers inline scripts; older appliance panels show a page whose buttons do nothing. |
| Auto Minify | **Off** | It rewrites HTML and JavaScript, breaking whitespace-sensitive templates. |
| Email Obfuscation | **Off** | It replaces addresses with scripts that can break admin pages. |
| Always Use HTTPS | **On** | webvpn also redirects and sends HSTS, but only after a browser has seen HTTPS. |

webvpn additionally forces `Cache-Control: private, no-store` on HTML responses as a second line of defense.

## Deploying with `push.py`

`push.py` is optional. It automates the full cycle and is designed to be safe against a live server.

Requirements:

```bash
pip install -r requirements.txt
```

Credentials are looked up in this order:

1. `$WEBVPN_CREDS`
2. `./creds.json` (ignored by git)
3. `~/.config/webvpn/creds.json`

File format:

```json
{
  "host": "192.168.1.50",
  "port": 22,
  "user": "root",
  "password": "..."
}
```

Then:

```bash
python push.py
```

What it does:

1. Cross-compiles `linux/amd64`.
2. Backs up the remote binary, unit and config with a timestamp.
3. Uploads the new binary as `.new` and runs `-check` against the **server's existing config**. If the new binary rejects it, the script stops before changing anything.
4. Appends missing `lan_index` and `state` blocks to the remote config, **append-only**. Existing content is never overwritten.
5. Uploads the unit, swaps the binary atomically, reloads and restarts.
6. Verifies `systemctl is-active`, that port 80 is bound to the LAN address rather than `0.0.0.0`, that the index returns 200 with cards, that `/admin` redirects (302), and that `state.json` is `webvpn:webvpn 600`.

Any failure triggers a rollback to the timestamped backups and prints the rollback paths.

It deliberately **does not upload your local `config.yaml`** (the server's copy has the real signing key and password hashes, and may have services the local file lacks), and **does not touch `state.json`**.

## Operations

Back up both files before upgrading:

```bash
sudo cp /opt/webvpn/config.yaml /opt/webvpn/config.yaml.bak-$(date +%F)
sudo cp /var/lib/webvpn/state.json /var/lib/webvpn/state.json.bak-$(date +%F)
```

Upgrade:

```bash
sudo install -o webvpn -g webvpn -m 0755 webvpn-linux-amd64 /opt/webvpn/webvpn.new
sudo -u webvpn /opt/webvpn/webvpn.new -check -config /opt/webvpn/config.yaml
sudo mv /opt/webvpn/webvpn.new /opt/webvpn/webvpn
sudo systemctl restart webvpn
```

If `-check` refuses the existing state (for example after a future state schema bump), do not overwrite it blindly; read the error, keep the backup, and consult the release notes.

### Rotating secrets

- **Signing key:** generate a new one, replace `session.signing_key`, restart. Everyone is logged out.
- **All sessions, same key:** bump `session.epoch`, restart.
- **Password:** change it on the admin page; it takes effect on the next login. Existing sessions stay valid until they expire or `epoch` is bumped.

## Next

- [LAN Index and Admin](LAN-Index-and-Admin) — the second listener and the admin UI
- [Security](Security) — what the deployment does and does not protect
- [Troubleshooting](Troubleshooting) — when something is running but not working
