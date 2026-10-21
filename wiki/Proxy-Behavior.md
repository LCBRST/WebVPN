# Proxy Behavior

**English** | [简体中文](Proxy-Behavior-zh)

This page describes what webvpn does to requests and responses, and what it deliberately does not do.

## Routing

Routing is by `Host` header, not by path:

- `server.portal_host` → portal: `/login`, `/logout`, and the service index at `/`.
- `<subdomain>.<base_domain>` → the matching service, after checking the session and the user's grant.
- Anything else → 404.

Because the path is preserved 1:1, `/api/x`, `/static/app.js` and WebSocket endpoints reach the backend unchanged.

### Why subdomains instead of a path prefix

A path-prefix design (`/proxy/192.168.1.10/`) requires rewriting every absolute URL in HTML, CSS and JavaScript, plus `Location`, cookies, `Range`, gzip and chunked encodings. Modern SPAs break in creative ways. Subdomain routing moves that cost to DNS and certificates, where it is a one-time setup problem.

The trade-off is that services share a parent domain. An XSS in one service can issue requests to another with the user's cookie; see [Security](Security).

## Request rewriting

The outbound request is built from scratch. Go deliberately drops inbound `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host` and `X-Forwarded-Proto` when a `Rewrite` hook is set, so a client cannot smuggle values through to a backend.

| Header | Behavior |
|---|---|
| `Host` | The target's host by default. With `preserve_host: true`, the public host is sent instead. |
| `X-Forwarded-Host` | The public host the browser used. |
| `X-Forwarded-Proto` | `https` when the public scheme is HTTPS, even though the hop to the backend is HTTP. |
| `X-Forwarded-Port` | `443` or `80`, matching `X-Forwarded-Proto`. |
| `X-Real-IP` | The client address supplied by Cloudflare (`CF-Connecting-IP`), not the raw connection. |
| `X-Forwarded-For` | Same address, unless `send_xff: false`. |
| `Cookie` | The webvpn session cookie is removed by name; the backend's own cookies stay. |
| `Origin` | Unchanged by default. With `rewrite_origin: true`, replaced with the origin the backend computes for itself. |

### `rewrite_origin`

The tunnel speaks plain HTTP to the origin. A backend that derives its origin from the connection sees `http://<its host>`, while the browser correctly sends `https://<public host>`. Applications that validate `Origin` reject the mismatch — often only on the WebSocket handshake, so the page loads but real-time features never connect.

`rewrite_origin: true` rewrites the request header to the backend's own origin. It also tells the backend that a cross-site request is same-origin, which is exactly what a CSRF check looks at. Enable it only for a trusted service that is otherwise unusable.

## Response rewriting

| Header | Behavior |
|---|---|
| `Set-Cookie` | The `Domain` attribute is removed, turning each cookie into a host-only cookie for the public subdomain. Browsers silently drop cookies whose `Domain` is not a parent of the current host, which is the usual "login does nothing" appliance bug. `__Host-` cookies are forced to `Secure; Path=/`; `__Secure-` cookies are forced to `Secure`; `SameSite=None` is upgraded to `Secure`. |
| `Location` | If it points at the backend or a host in `extra_location_hosts`, it becomes a relative reference. This keeps the browser on the public host instead of sending it to an unreachable LAN address or around the login. |
| `Refresh` | Same treatment for the legacy `Refresh: 0; url=...` header. |
| `Access-Control-Allow-Origin` | With `fix_cors: true`, a value naming the backend's own address is replaced by the request's `Origin`, and `Vary: Origin` is added. `*` is left alone, and credentials plus `*` is never synthesized. |
| `Content-Type: text/html` | `Cache-Control: private, no-store` is forced, so Cloudflare cannot serve one session's page to another. |
| `Alt-Svc`, `Public-Key-Pins` | Removed; they advertise endpoints nobody can reach. |

`rewrite_location` and `fix_cors` default to off/on as documented in [Configuration](Configuration#services). `rewrite_location` defaults to **on**.

## WebSockets, SSE and long-polling

- WebSocket upgrades work because the logging middleware forwards `Hijack`, `Flush` and `Unwrap` to the underlying writer. Without `Hijack`, upgrades fail with "response does not implement http.Hijacker" while ordinary requests keep working.
- There is deliberately no write timeout. Both `http.Server` instances leave `WriteTimeout` at zero; any non-zero value cuts SSE and long-poll streams mid-response, and WebSocket hides the bug because hijacked connections ignore it.
- Cloudflare's edge closes idle connections after roughly 100 seconds. VNC and terminal services need application-level heartbeats.

## What cannot be fixed

webvpn does not rewrite response bodies. Absolute URLs embedded in HTML or JavaScript are outside its reach:

```html
<img src="http://192.168.1.1/logo.png">
<base href="http://192.168.1.1/">
<script>const API = "http://192.168.1.1/api";</script>
```

Fix these in the backend's own configuration:

| Application | Settings |
|---|---|
| Nextcloud | `overwrite.cli.url`, `overwritehost`, `trusted_proxies` |
| Home Assistant | `http.external_url`, `http.trusted_proxies`, `http.use_x_forwarded_for` |
| Grafana | `root_url` |
| GitLab | `external_url` |

Use the service's `note` field to record quirks like these on the portal card.

## Service option cheat sheet

| Symptom | Setting |
|---|---|
| Backend returns 403 because it sees a public IP in `X-Forwarded-For` | `send_xff: false` |
| Redirect goes to an internal IP or host name | add it to `extra_location_hosts` |
| Page loads but the backend never connects / real-time data spins | `rewrite_origin: true` |
| Browser rejects a cookie from the backend | normally automatic; check for `__Host-` requirements |
| Backend serves HTTPS with a self-signed certificate | `insecure_tls: true` (logs a warning) |
| Backend is vhost-style and needs a specific SNI name | `tls_server_name` |
| Backend expects its public host in `Host` | `preserve_host: true` |
| CORS response names the backend's address | `fix_cors: true` |
| Card link in the LAN index is wrong | `lan_url` |

## Testing a new service

1. Add it on the admin page (or in the seed config before first start).
2. Open the portal and click the card; log in if prompted.
3. Verify a deep link works (e.g. `/admin/settings`), not just the root.
4. If the app uses WebSockets, watch the browser console and backend logs during the handshake.
5. If login appears to succeed then bounce back, inspect the backend's `Set-Cookie` in devtools; a `Domain` that webvpn failed to strip (unusual) or a `__Host-` contract is the likely cause.
6. Run `webvpn -v` for debug-level logs while testing.
