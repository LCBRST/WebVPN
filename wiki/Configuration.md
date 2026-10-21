# Configuration

**English** | [简体中文](Configuration-zh)

webvpn reads two files, and knowing which one owns what is the main thing to get right:

| File | Contains | Changed by |
|---|---|---|
| `config.yaml` | Listener, domain, session, login policy, LAN index, seed users/services | You, then restart |
| `state.json` | The live service list, user list and per-user access rules | The LAN admin page, immediately |

`config.yaml` is a hand-written file: the program never rewrites it, so comments and ownership survive. `state.json` is program-owned and is written atomically with mode `0600` because it contains bcrypt hashes.

Unknown keys are a hard error. `-check` validates everything without starting the server or writing state.

## `server`

| Key | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:8080` | `host:port` for the tunnel-facing listener. Keep it on loopback; `cloudflared` on the same machine is the only client that needs it. |
| `base_domain` | required | Zone apex. Service URLs are `<subdomain>.<base_domain>`, and the session cookie's `Domain` is set to this value. |
| `portal_host` | required | Host for the login page and service index. Must be `base_domain` or a subdomain of it. A wildcard tunnel rule does not match the apex, so this needs its own DNS record and ingress rule. |
| `public_scheme` | `https` | Scheme browsers use. `http` is for local hosts-file testing only; over a tunnel it would ship session cookies in the clear. |
| `allow_loopback_targets` | `true` | Permit services whose target is `127.0.0.0/8` or `::1`, the normal case when a backend runs on the same host. |
| `read_header_timeout` | `10s` | HTTP read-header timeout. |
| `idle_timeout` | `120s` | HTTP idle timeout. |

There is deliberately **no `write_timeout` key**. It is hardcoded to zero: any non-zero value cuts server-sent events and long-polls off mid-stream, and WebSocket traffic hides the bug because hijacked connections ignore it.

## `session`

| Key | Default | Meaning |
|---|---|---|
| `cookie_name` | `webvpn_session` | Cookie name. Use something uncommon; an appliance on the same device may otherwise set a colliding name. |
| `signing_key` | required | HMAC key. `webvpn -genkey` prints a 32-byte base64 value. A raw passphrase of at least 16 bytes is also accepted. |
| `ttl` | `168h` | Lifetime of a freshly issued token (7 days). |
| `refresh_after` | `84h` | Reissue the token once it is this close to expiry, turning a fixed deadline into a sliding one for active users. Must be positive and less than `ttl`. |
| `epoch` | `1` | Embedded in every token. Increment and restart to invalidate every session ever issued. |

The signing key can also be supplied as `WEBVPN_SESSION_SECRET`, which overrides the file and keeps it out of backups and shell history.

The session cookie is set with `Domain=<base_domain>`, so it is shared by the portal and every service host. The cookie is `HttpOnly`, `SameSite=Lax`, and `Secure` when `public_scheme` is `https`.

## `state`

| Key | Default | Meaning |
|---|---|---|
| `path` | `state.json` next to the config | JSON file holding mutable state. Must be writable. |

In production this must point somewhere writable. The systemd unit sets `StateDirectory=webvpn` (which creates `/var/lib/webvpn`, owned by the service user, mode `0700`) and `ProtectSystem=strict` makes `/opt/webvpn` read-only, so the intended value is:

```yaml
state:
  path: "/var/lib/webvpn/state.json"
```

If the directory does not exist, startup fails with an error naming it; the unit is what creates it.

## `users` (seed)

| Key | Meaning |
|---|---|
| `name` | Unique, at most 64 bytes, no leading or trailing whitespace. |
| `password_hash` | bcrypt hash. Generate with `webvpn -hash`; a plaintext value is refused. |
| `admin` | Grants the LAN admin page and, implicitly, every service. At least one admin must exist in the state. |
| `services` | Ignored when seeding. See below. |

At least one user is required in `config.yaml`. If none has `admin: true`, the first user is promoted at startup and a warning is logged; a state with no admin cannot be edited back through the UI, so the store refuses to persist one.

The `services` field is accepted but **ignored during seeding**: every user is granted every service, preserving pre-ACL behavior across an upgrade. Grants are edited on the admin page afterwards.

## `login`

| Key | Default | Meaning |
|---|---|---|
| `attempts_per_minute` | `5` | Per-client token bucket for login POSTs. |
| `max_concurrent` | `1` | Password comparisons allowed at once. bcrypt at cost 12 is roughly 250 ms of CPU, so this prevents a burst from saturating the machine. |
| `max_failures` | `5` | Wrong passwords within `failure_window` before the client is locked out. |
| `failure_window` | `15m` | Window for counting failures. |
| `lockout` | `15m` | First lockout duration. Each further lockout without a successful login doubles it. |
| `max_lockout` | `4h` | Cap on the doubling. |

Lockouts are keyed by client address, not user name, so an attacker cannot lock a real account by guessing its name. IPv6 clients are counted per `/64`. The state is in process memory; restarting clears it.

## `lan_index`

| Key | Default | Meaning |
|---|---|---|
| `listen` | `""` (disabled) | `host:port` for the second listener. Bind a LAN address, never `0.0.0.0`. |
| `host` | `""` | Host used to replace loopback targets in links. Required if any service targets loopback. Must be a bare host name or IPv4 address, no scheme or port. |

The LAN listener serves two things and nothing else: the unauthenticated index at `/` and the admin page at `/admin`. It cannot reach the proxy or any backend. Ports below 1024 need `CAP_NET_BIND_SERVICE`; see [Deployment](Deployment#systemd).

See [LAN Index and Admin](LAN-Index-and-Admin) for the full behavior and caveats.

## `services`

Only `name`, `subdomain` and `target` are required.

| Key | Default | Meaning |
|---|---|---|
| `name` | required | Display name on cards and in the admin page. |
| `subdomain` | required | Lowercase DNS label (`[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`), unique, and not equal to the portal's label. |
| `target` | required | `http` or `https` URL with a host and **no path**. A path prefix would break the 1:1 mapping. |
| `description` | `""` | Shown on the portal card. |
| `note` | `""` | Free-form note shown on the card; useful for per-service quirks. |
| `insecure_tls` | `false` | Skip backend certificate verification. Logs a warning at startup. |
| `tls_server_name` | `""` | SNI/verification name for a vhost-style HTTPS backend. |
| `preserve_host` | `false` | Send the public Host to the backend instead of the target's host. Most appliances want the default. |
| `send_xff` | `true` | Send `X-Forwarded-For`. Turn off for appliances that use it for their own IP allowlist and would see a public address. |
| `strip_session_cookie` | `true` | Remove webvpn's session cookie before forwarding. Keep this on. |
| `rewrite_location` | `true` | Rewrite `Location` and `Refresh` URLs that point at the backend into relative references. |
| `extra_location_hosts` | `[]` | Additional host names that mean "the backend" in redirects, e.g. `router.lan` or `localhost:3000`. |
| `fix_cors` | `false` | Rewrite an `Access-Control-Allow-Origin` response header that names the backend's own address. |
| `rewrite_origin` | `false` | Rewrite the request's `Origin` to the backend's own origin. Needed by apps that validate Origin and reject the tunnel's scheme mismatch. This also defeats their CSRF check, so enable it only for trusted services. |
| `lan_url` | `""` | Override the link the LAN index uses for this service. |
| `allow_loopback` | — | Not a per-service key; controlled globally by `server.allow_loopback_targets`. |

### Validation rules

A service is refused if:

- `name` is empty.
- `subdomain` is not a lowercase DNS label, is duplicated, or collides with the portal host's label.
- `target` is not `http`/`https`, has no host, includes a path, points at webvpn's own listener, or is a literal public IP address.

Host names are resolved later, and the resolved address is checked before connecting. That catches names pointing at public addresses and DNS rebinding; see [Security](Security).

`config.yaml` must contain at least one service, but the admin page may delete the last one — an empty live list is a valid state.

## Seeding and `state.json`

On first start, if `state.json` does not exist, webvpn writes it from the config:

- every user and service is copied;
- every user is granted every service;
- if nobody is an admin, the first user is promoted;
- the file is written atomically and logged.

After that, `config.yaml`'s `users:` and `services:` are ignored. Editing them has no effect until you delete `state.json` (which also discards every admin-page change) or edit through the admin page.

`state.json` carries a schema version. A file from a newer build is refused rather than misread. Back it up before upgrading.

## What needs a restart

| Change | Restart? |
|---|---|
| Services, users, grants, passwords (admin page) | No |
| `server`, `session`, `login`, `lan_index` | Yes |
| Per-service flags edited in `config.yaml` | Yes, unless done through the admin page |
| Bumping `session.epoch` | Yes |
| Rotating the signing key | Yes (all sessions invalidated) |
