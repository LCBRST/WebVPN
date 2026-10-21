# Wiki source

These files are the source for the GitHub wiki at
<https://github.com/LCBRST/WebVPN/wiki>.

Each `.md` file maps to a wiki page of the same name:

| File | Page |
|---|---|
| `Home.md` | landing page, bilingual |
| `Installation.md` / `Installation-zh.md` | installation |
| `Configuration.md` / `Configuration-zh.md` | configuration reference |
| `Deployment.md` / `Deployment-zh.md` | production deployment |
| `LAN-Index-and-Admin.md` / `LAN-Index-and-Admin-zh.md` | LAN listener and admin page |
| `Proxy-Behavior.md` / `Proxy-Behavior-zh.md` | rewriting and protocol behavior |
| `Security.md` / `Security-zh.md` | security model |
| `Troubleshooting.md` / `Troubleshooting-zh.md` | symptom table |

## Editing

- Keep the English and Chinese versions in sync when changing behavior.
- Page names are the link targets: `[Configuration](Configuration)` from anywhere in
  the wiki, or `Configuration-zh` for the Chinese page.
- `README.md` (this file) and `publish.sh` are not published as wiki pages.

## Publishing

The wiki repository is separate from the main repository. After the first page
exists (created once in the GitHub UI), run:

```bash
./wiki/publish.sh
```

or manually:

```bash
git clone https://github.com/LCBRST/WebVPN.wiki.git /tmp/webvpn-wiki
for f in wiki/*.md; do
  [ "$(basename "$f")" = "README.md" ] || cp "$f" /tmp/webvpn-wiki/
done
cd /tmp/webvpn-wiki
git add -A && git commit -m "Sync wiki from main repository" && git push
```

Do not copy `README.md` or `publish.sh` into the wiki repository; they are
repo-maintenance files, not wiki pages.
