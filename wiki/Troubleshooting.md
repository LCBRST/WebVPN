# Troubleshooting

**English** | [简体中文](Troubleshooting-zh)

Start with the logs: `journalctl -u webvpn -f`, or run `webvpn -v -config config.yaml` in a terminal. `-check` catches configuration problems before startup; unknown keys are errors, not warnings.

## Login and sessions

| Symptom | Cause and fix |
|---|---|
| Login succeeds, then the next page bounces back to login | The backend set a cookie with `Domain=<internal IP>`; webvpn normally strips it. If it still happens, check for a `__Host-` cookie contract or a second cookie with the same name. |
| Random logouts on some devices | Cookie name collision: an appliance admin panel sets a cookie with the same name, and the browser sends both. Use an uncommon `session.cookie_name`; webvpn also strips its own cookie from forwarded requests. |
| Correct password is refused during a lockout | Intended. Failure lockout persists server-side for `login.lockout`, doubling on repeat, and is keyed by client address. Wait, restart, or raise the limits. |
| Everyone is logged out after an upgrade | `session.signing_key` changed, or `session.epoch` was bumped. Restore the key or accept re-login. |
| Login page loads but POST fails | Check `public_scheme`. Over a tunnel it must be `https`, or the cookie is not `Secure` and the redirect logic misbehaves. |
| Admin page login bounces immediately | You opened `/admin` through a different host name than the one you logged in on (IP vs name vs alias). The admin cookie is host-only; use one host consistently. |
| Forgot the admin password | Stop the service, back up `state.json`, replace a user's `password_hash` with output from `webvpn -hash` (or add `"admin": true`), restart. Make sure at least one admin remains. |

## Proxied applications

| Symptom | Cause and fix |
|---|---|
| Page loads but every button does nothing | Cloudflare Rocket Loader or Auto Minify is rewriting HTML/JS. Turn both off. |
| Backend returns 403 after login | The appliance trusts `X-Forwarded-For` for its IP allowlist and sees a public address. Set `send_xff: false`. |
| Redirect goes to an internal IP and cannot load | The backend hardcodes an absolute redirect. Add that host to `extra_location_hosts`; if it is in HTML/JS, configure the backend (see [Proxy Behavior](Proxy-Behavior#what-cannot-be-fixed)). |
| Page opens but real-time data spins forever | The app validates `Origin` and rejects the HTTPS→HTTP mismatch, usually on the WebSocket handshake. Set `rewrite_origin: true` for this trusted service. `fix_cors` does not help; it changes a response header. |
| WebSocket fails with "does not implement http.Hijacker" | Should not happen with current builds; the middleware forwards `Hijack`. If it does, check that nothing else wraps the response writer. |
| SSE stalls then arrives all at once | A `WriteTimeout` was introduced somewhere. Both servers must leave it at zero. |
| VNC/ttyd disconnects after about a minute | Cloudflare closes idle connections around 100 seconds. Use application-level heartbeats. |
| Login succeeds but returns to the login page repeatedly (Synology DSM and similar) | Add webvpn's address to the appliance's trusted-proxy list. |
| CORS errors in the browser console | Set `fix_cors: true` if the response names the backend's address. If the request is rejected instead, see `rewrite_origin`. |
| Backend HTTPS uses a self-signed certificate | Set `insecure_tls: true`; startup logs a warning. Prefer installing a trusted certificate on the backend. |

## Configuration and startup

| Symptom | Cause and fix |
|---|---|
| `unknown field "subdomian"` | `-check` is strict on purpose. Fix the spelling. |
| `target must not include a path` | Path-prefix targets are unsupported; the mapping is 1:1 by subdomain. |
| `target is not a private address` | Targets must be private or loopback. This prevents webvpn from becoming an open relay. |
| `users[0] password_hash is not a bcrypt hash` | Generate it with `webvpn -hash`; do not paste plaintext. |
| `at least one user must have admin: true` | The state would be impossible to administer. Promote an admin. |
| `session.signing_key must be at least 16 bytes` | Run `webvpn -genkey`; the key is 32 bytes base64. |
| `lan_index.host is required` | A service targets loopback. Set `lan_index.host` or that service's `lan_url`. |
| `state.path` write fails / read-only file system | The default is next to the config. In production set `/var/lib/webvpn/state.json`; the unit's `StateDirectory` creates it. |
| Changes to `config.yaml` users/services do nothing | Normal once `state.json` exists. Use the admin page, or delete `state.json` to re-seed (this discards admin-page changes). |
| `version N is not supported by this build` | The state file is from a newer build. Use the matching binary or restore a backup. Do not hand-edit blindly. |

## LAN index and admin page

| Symptom | Cause and fix |
|---|---|
| LAN index unreachable | Check `ss -lntp \| grep ':80 '`; the address must be the LAN IP, not `0.0.0.0`. Check the firewall (`ufw status`). |
| `bind: permission denied` | Port below 1024 without capabilities in the unit. `setcap` is silently ignored under `NoNewPrivileges=true`; grant it via `AmbientCapabilities` and `CapabilityBoundingSet`. |
| A card points at `127.0.0.1` and fails from another machine | Loopback target without `lan_index.host` or `lan_url`. |
| Index page is reachable from the internet | `listen` is `0.0.0.0`. Rebind to the LAN address. |
| `/admin` returns 404 | The LAN listener is disabled (`lan_index.listen` empty), or you are on the wrong host/port. |

## Deployment and systemd

| Symptom | Cause and fix |
|---|---|
| Service exits immediately with a state write error | `state.path` points into `/opt/webvpn`, which `ProtectSystem=strict` makes read-only. Set `/var/lib/webvpn/state.json`. |
| Service cannot read `config.yaml` | Ownership or mode after editing with an editor that replaces the file (`vi`, `sed -i`): `chown webvpn:webvpn` and `chmod 600`. |
| `webvpn -check` succeeds as root, service fails as `webvpn` | The service user cannot read the config or write the state directory. Fix ownership, not content. |
| New binary starts then fails on state | Run `webvpn.new -check -config ...` first; `push.py` does this automatically. Keep a backup. |

## Cloudflare and the tunnel

| Symptom | Cause and fix |
|---|---|
| Tunnel is `active (running)` but dashboard changes never apply | Fake-IP DNS: the router resolves `*.argotunnel.com` to `198.18.0.x` (Clash/Mihomo default fake-IP range), so cloudflared cannot reach the edge and the config version freezes. Try `--protocol http2` (TCP 7844) and add direct rules (below). |
| Local DNS answers `198.18.0.x` or `2001:2::x` | That is a proxy fake IP, not real. Use DoH to check records: `https://1.1.1.1/dns-query?name=X&type=A`; `"Status":3` means NXDOMAIN. |
| Two-label hostname fails with `ERR_SSL_VERSION_OR_CIPHER_MISMATCH` | Universal SSL covers one label only. Rename to one label under the apex, or buy ACM / upload a wildcard certificate. |
| Tunnel routes the name but the browser rejects the certificate | Ingress matching is not certificate coverage. Inspect the certificate chain. |
| Logged-in page served to someone else | Add a Cache Rule with **Bypass cache** for these hostnames. |

### Keep tunnel traffic off the proxy

On a LAN with transparent proxying (Clash/Mihomo TUN mode), tunnel traffic can be routed abroad and back. Besides wasting bandwidth, a failed proxy node cuts off all remote access. Put these rules **first** in the proxy config:

```yaml
rules:
  - DOMAIN-SUFFIX,argotunnel.com,DIRECT
  - IP-CIDR,198.41.192.0/24,DIRECT,no-resolve
  - IP-CIDR,198.41.200.0/24,DIRECT,no-resolve
```

Notes:

- The domain rule works under fake-IP mode; the proxy remembers which domain a fake IP maps to.
- `no-resolve` stops the proxy from resolving the domain just to match an IP rule.
- If the config comes from a subscription, edit it through the client's override/mixin feature; a hand edit is overwritten on the next subscription update.
- Changing DNS does not help: TUN mode matches rules regardless of the resolved address.

Check the proxy's connections page for `argotunnel`; `chains` should show `DIRECT`.

## Still stuck?

Open an issue with:

- the output of `webvpn -check -config ...`,
- relevant log lines with secrets removed,
- the service entry (with the real address replaced by a placeholder),
- what the browser console and backend logs show during the failing request.
