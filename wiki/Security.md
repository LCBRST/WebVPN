# Security

**English** | [简体中文](Security-zh)

This page states what webvpn protects, how, and where the boundaries are.

## Trust boundary

```
Internet ──TLS──> Cloudflare ──tunnel──> cloudflared ──plain HTTP──> webvpn ──plain HTTP──> LAN
                                       ^ same host, loopback only
```

The public listener binds `127.0.0.1`. The only process that can reach it is the local `cloudflared`, so headers it sets (`CF-Connecting-IP`, `CF-Visitor`, `X-Forwarded-Proto`) can be trusted. Other LAN machines can neither spoof nor reach the login page.

The LAN listener is separate. It trusts the kernel's `RemoteAddr`, refuses non-private clients, and has its own session key derived as `HMAC(sessionKey, "webvpn-lan-admin")`.

## Transport

- `public_scheme: https` adds `Strict-Transport-Security: max-age=15552000; includeSubDomains` to public responses and 308-redirects plain-HTTP requests, including login POSTs, to HTTPS.
- The scheme decision uses `CF-Visitor` / `X-Forwarded-Proto`, not `r.TLS`; the origin hop is always plain HTTP.
- HSTS only applies after a browser has seen HTTPS once, so Cloudflare's **Always Use HTTPS** should also be on.
- TLS terminates at the Cloudflare edge. Cloudflare can see request contents. That is inherent to using a Tunnel and is a trust assumption, not a bug.

## Authentication and authorization

**Login answers "who are you"; authorization answers "may you reach this subdomain".** They are separate:

- Session tokens are stateless: HMAC-SHA256 over a payload containing the user name, expiry, epoch and a random nonce. They carry no permission set.
- Every proxied request re-reads the user's grants from the store. Revoking a grant or deleting a user takes effect on the next request, without a restart and without waiting for the cookie to expire.
- `session.epoch` is the "log everyone out" switch. Bumping it invalidates every token, at the cost of logging out every user.
- Passwords are bcrypt at cost 12. `-hash` reads from stdin so the password never appears in `argv` or shell history.

## Login hardening

Four layers, applied to both the portal and the admin login (with independent limiter state):

1. **Failure lockout** per client address. `max_failures` wrong passwords inside `failure_window` lock the address for `lockout`; repeated lockouts without a success double it up to `max_lockout`. State is server-side, so refreshing does not clear it. IPv6 is counted per `/64`.
2. **Token bucket** per client address (`attempts_per_minute`).
3. **Concurrency semaphore** (`max_concurrent`), because bcrypt comparisons are a CPU bomb.
4. **Dummy hash comparison** for unknown user names, so response time does not enumerate valid accounts.

Lockouts are keyed by address, not user name: otherwise an attacker could lock a real account by guessing its name. The cost is that clients behind one NAT share a lockout, and distributed attacks are only slowed by the token bucket.

The admin login uses the kernel-provided `RemoteAddr`, not forwarding headers, because anyone on the LAN can set those.

## SSRF and upstream validation

- `target` must be `http` or `https`, have a host, and have no path.
- A literal public IP target is refused at config load.
- A host name is resolved and checked in `Dialer.Control` — after DNS resolution, before `connect`. This also defeats DNS rebinding, which a config-time check cannot.
- Only private and loopback ranges are allowed (loopback is opt-in globally via `allow_loopback_targets`).
- A target pointing at webvpn's own listener is refused, preventing a loop.

**The one hard rule: never add a "proxy an arbitrary URL" feature** (for example a `?url=` parameter). It would turn the allowlist into an open SSRF relay.

## Cookie handling

- The webvpn session cookie is stripped before forwarding to any backend, so appliances never receive a credential they could replay.
- Backend `Set-Cookie` headers have `Domain` removed and browser-enforced prefixes repaired; see [Proxy Behavior](Proxy-Behavior#response-rewriting).
- Cookies are `HttpOnly`, `SameSite=Lax`, and `Secure` when the public scheme is HTTPS.
- The session cookie's `Domain` is the base domain, so it is sent to every service subdomain and to unrelated hosts under the same apex (for example an RTMP host that is not HTTP). Bumping `epoch` is the remedy if a token leaks.

## Admin page

- Only `admin: true` users pass the login form. Non-admins receive 401 even with correct credentials.
- Mutating requests validate `Origin`, falling back to `Referer`, against the current host; if both are absent the request is refused. Combined with `SameSite=Lax`, this is the CSRF defense. There is deliberately no per-form CSRF token; the reasoning is in `sameOrigin`'s comment.
- The admin listener is plain HTTP. Its cookie crosses the LAN in the clear. This is a conscious trade-off for keeping administration off the internet.

## Logging

Access logs include host, method, path, status, duration and client IP. They deliberately omit:

- query strings (many appliance panels put tokens there),
- all headers,
- cookies and `Set-Cookie`,
- `Authorization`.

Panic logs include a stack trace but no request headers.

## Known limitations

These are real; do not deploy under the assumption that webvpn removes them.

1. **ACL covers the tunnel only.** Backends are directly reachable on the LAN. Anyone on the same network can bypass the index and the proxy. The ACL means "what the internet path carries", not "what is visible".
2. **XSS is shared across services.** All services live under one parent domain. A cross-site scripting bug in one service can issue requests to another with the victim's session cookie. Do not put untrusted applications behind this proxy.
3. **Stateless sessions cannot be revoked individually.** Deleting a user works because grants are re-read; but a stolen token remains cryptographically valid until it expires or `epoch` changes.
4. **Cloudflare sees plaintext.** Tunnel terminates TLS at the edge. Self-hosting does not make Cloudflare blind.
5. **The LAN admin page is plaintext HTTP.** Same-segment capture can steal its cookie.
6. **No audit log or alerts.** Logs exist, but nothing detects or notifies on suspicious activity.
7. **Not independently audited.** Treat it as a well-commented personal tool, not a certified security product.

## Recommendations

- Keep `listen` on `127.0.0.1` and the LAN listener on a private address.
- Use a long random password for every admin, and `admin: true` sparingly.
- Keep `public_scheme: https`; enable Cloudflare's Always Use HTTPS.
- Back up `state.json` before upgrades; it contains every password hash and grant.
- Rotate `session.signing_key` if it may have been exposed; bump `epoch` after any suspicion.
- Never commit `config.yaml`, `state.json` or `creds.json`.
