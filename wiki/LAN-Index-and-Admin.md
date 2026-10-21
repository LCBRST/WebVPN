# LAN Index and Admin

**English** | [简体中文](LAN-Index-and-Admin-zh)

webvpn can open a second listener that is reachable only from the LAN. It serves exactly two things and nothing else:

- `GET /` — an unauthenticated card list linking directly to each service's LAN address.
- `/admin` — a login-protected page for managing services, users and access.

It cannot reach the reverse proxy or any backend. There is no route on this listener that forwards traffic.

## Why it exists

Reaching a service from inside the house should not require leaving the LAN, going through Cloudflare and logging in. The LAN index is one click. The admin page exists because adding a service or granting access should not mean editing YAML, running `-check`, fixing file ownership and restarting.

## Configuration

```yaml
lan_index:
  listen: "192.168.1.50:80"
  host: "192.168.1.50"
```

| Key | Meaning |
|---|---|
| `listen` | `host:port` to bind. Empty disables the listener. |
| `host` | Host used to replace loopback targets in links. Required if any service targets `127.0.0.1` or `::1`. |

Bind a LAN address, **not `0.0.0.0`**. On a machine with a public address, `0.0.0.0` publishes the list of everything you run to the internet. The code refuses clients whose kernel-provided address is not private, but that is a backstop; the bind address is the real control.

Ports below 1024 need `CAP_NET_BIND_SERVICE` from the systemd unit. See [Deployment](Deployment#systemd).

### Loopback targets

A browser on another machine resolves `127.0.0.1` to **itself**, so a card pointing at `http://127.0.0.1:3000` is a dead link. Setting `lan_index.host` swaps the host while keeping the scheme and port. If a service targets loopback and `host` is empty, startup fails rather than serving a page of dead links.

Use `lan_url` on an individual service to override the generated link:

```yaml
  - name: "Grafana"
    subdomain: "grafana-vpn"
    target: "http://127.0.0.1:3000"
    lan_url: "http://192.168.1.50:3000/"
```

## Security model of this listener

- It has its own mux. Every other path is a 404.
- It does not trust `CF-Connecting-IP` or `X-Forwarded-For`; any LAN client can set those. Logs use the kernel's `RemoteAddr`, and non-private clients are refused.
- The admin session uses a **derived key**, `HMAC(sessionKey, "webvpn-lan-admin")`, and its own cookie. It is not interchangeable with a portal session, and vice versa. This is structural, not an `if` check.
- The admin cookie has no `Domain` attribute and is not `Secure` (the LAN listener is plain HTTP). It is scoped to the exact host you used to open the admin page.

**The listener is plain HTTP.** The admin session cookie crosses the LAN in the clear, and someone on the same network can capture it. That is the inherent cost of keeping the admin page off the internet. If that is unacceptable, put TLS in front of this port or move administration to the tunnel side (which gives up "LAN only").

## Using the admin page

Open `http://<lan-ip>/admin`. Only users with `admin: true` can log in; a valid non-admin account is refused at the login form with 401.

You can:

- add, edit and delete services;
- add and delete users;
- change passwords;
- tick which services each user may reach.

Changes apply immediately and are written to `state.json` atomically.

### Bootstrap: the first admin

A state with no admin cannot be edited back into having one through the UI, so:

- If the config file has no user with `admin: true`, the first user is promoted at startup and a `WARN` is logged.
- The store refuses to persist a state with no admin, so the UI will not let you delete or demote the last one.

After the first start, open the admin page and make sure the right account is the admin.

### Seeding is grant-all

The first time `state.json` is created, every user is granted every service. This preserves pre-ACL behavior and avoids an upgrade silently breaking someone. Tighten access afterwards in the admin page. The `services:` allowlist written in `config.yaml` is ignored during seeding.

## Access control semantics

- **Default deny.** A user reaches a service only if their grant list contains its subdomain.
- **Admins implicitly reach everything.** On the admin page, the checkboxes for an admin show the *stored* list, not effective access. This matters: if the UI showed "all checked" for an admin and you saved, it would write an all-granted list for everyone's expectations without you intending it.
- **Every request re-checks.** The session token carries a user name, never a permission set. Revoking a grant or deleting an account takes effect on the very next request — the still-valid cookie cannot help.
- **Deleting a user does not invalidate the cookie.** It does not need to: authorization is re-read each time, and an unknown user gets nothing.
- **The ACL only covers the tunnel.** Backends are directly reachable on the LAN, so someone on the same network can bypass the index. The semantics are "what the internet path will carry for you", not "what you can see". This is why the LAN index lists everything regardless of grants.

## Common pitfalls

| Symptom | Cause |
|---|---|
| Started but `bind: permission denied` | Port below 1024 without capabilities in the unit. `setcap` is silently ignored under `NoNewPrivileges=true`. |
| `lan_index.host is required` at startup | A service targets loopback. Set `lan_index.host` or that service's `lan_url`. |
| A card points at `127.0.0.1` and does not open from another machine | Loopback target without `host`/`lan_url`. |
| Logged in to `/admin`, next click bounces back to login | You changed host (IP vs hostname vs alias). The admin cookie is host-only, so each host is a separate session. |
| Index page reachable from the internet | `listen` is `0.0.0.0`; rebind to the LAN address and restart. |

## Limits

- The index is unauthenticated by design. Anyone on the same segment sees service names and descriptions. They contain no credentials and grant no proxy access, but the list itself is visible.
- The admin page is the keys to the whole VPN. Use a strong password; do not hand out `admin: true` casually.
- ACL changes do not restrict direct LAN access to a backend. Network segmentation is a separate problem.
