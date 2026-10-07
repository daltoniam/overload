# Cloudflare Tunnel for GitHub webhooks

GitHub delivers pull request events to a public `https://` URL. A Mac or a
home server usually has no public address, so overload uses a
[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/):
`cloudflared` connects out to Cloudflare, and Cloudflare forwards requests
for one hostname back through that connection. No port is opened on your
router.

Only `/webhooks/github` is forwarded. Every other path, including the UI,
gets a 404 from Cloudflare and never reaches your machine. Webhooks are
signed, so the webhook path is safe to expose; the UI is not meant to be
public.

You need a Cloudflare account with a domain whose DNS is on Cloudflare (the
free plan is enough).

## One click on a Mac

1. Install cloudflared: `brew install cloudflared`.
2. In overload open **GitHub → Set up Cloudflare Tunnel** (`/setup/tunnel`).
3. **Log in to Cloudflare**. Your browser opens Cloudflare; pick the domain
   to use. This saves `~/.cloudflared/cert.pem`.
4. Enter a hostname on that domain, for example `overload.example.com`, and
   click **Create tunnel**.

Overload then:

- creates a tunnel named `overload` (or reuses one with that name whose
  credentials are on this Mac),
- creates the DNS record for the hostname,
- writes `cloudflared/config.yml` in the overload install folder
  (`~/Library/Application Support/overload`) that forwards only the webhook,
- runs `cloudflared` as a login service (`dev.overload.tunnel`), with its
  log in `logs/tunnel.log`,
- checks from the internet that the webhook answers and the UI does not,
- points your GitHub App's webhook at the new URL, if the App was created in
  overload.

If you create the GitHub App afterwards, the tunnel's webhook URL is filled
in for you. **Stop the tunnel service** removes the login service; the
tunnel and DNS record stay in Cloudflare so you can start it again.
`overload uninstall` also removes the tunnel service.

## By hand (Linux, or a custom setup)

```sh
cloudflared tunnel login
cloudflared tunnel create overload
cloudflared tunnel route dns overload overload.example.com
```

Write `~/.cloudflared/config.yml` (use the tunnel ID that `create` printed):

```yaml
tunnel: <tunnel-id>
credentials-file: /home/you/.cloudflared/<tunnel-id>.json
ingress:
  - hostname: overload.example.com
    path: ^/webhooks/github$
    service: http://127.0.0.1:8082
  - service: http_status:404
```

Run it as a service:

- **Linux:** `sudo cloudflared service install` copies the configuration to
  `/etc/cloudflared` and installs a systemd unit (`systemctl status
  cloudflared`).
- **macOS:** `cloudflared service install` writes a launch agent that may
  start `cloudflared` with no arguments and exit at once. If
  `launchctl list | grep cloudflared` shows a non-zero status, set
  `ProgramArguments` in `~/Library/LaunchAgents/com.cloudflare.cloudflared.plist`
  to `/opt/homebrew/bin/cloudflared --no-autoupdate --config
  /Users/you/.cloudflared/config.yml tunnel run` and reload it with
  `launchctl unload` and `launchctl load`. The one-click setup avoids this.

## Check it

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://overload.example.com/webhooks/github   # 401: reaches overload, unsigned
curl -s -o /dev/null -w '%{http_code}\n' https://overload.example.com/                         # 404: UI not exposed
```

Use `https://overload.example.com/webhooks/github` as the GitHub App's
webhook URL (GitHub sends JSON; App webhooks have no content-type setting).
Recent deliveries and their results are on overload's **Webhooks** page and
in the App's settings under **Advanced**.
