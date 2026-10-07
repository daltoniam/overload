# Install overload on a Linux server

This guide runs overload on a small cloud server (a DigitalOcean Droplet,
an AWS EC2 instance or any Linux VM) with **hosted models**: the server only
calls a model API, so it needs little memory or CPU. For Kubernetes, see
[install-kubernetes.md](install-kubernetes.md). Local models are meant for a
Mac or a home machine with a large GPU; see [install-mac.md](install-mac.md).

You need:

- A Linux VM: Ubuntu 24.04 or Debian 12, x86-64 or ARM, 1 vCPU and 2 GB of
  memory are enough. Each review downloads the pull request's code to `/tmp`,
  so leave a few GB of disk free.
- Postgres 14 or later: a managed database (DigitalOcean Managed PostgreSQL,
  Amazon RDS) or Postgres on the same VM.
- An API key for an OpenAI-compatible model provider.
- A hostname for webhooks, either on Cloudflare (for a tunnel, no open ports)
  or pointed at the VM (for a reverse proxy with HTTPS).

## 1. Install the binary

```sh
curl -fsSL https://raw.githubusercontent.com/daltoniam/overload/main/install.sh \
  | sudo OVERLOAD_NO_BREW=1 OVERLOAD_BINARY_ONLY=1 OVERLOAD_INSTALL_DIR=/usr/local/bin sh
overload version
```

The script downloads the release for your CPU, checks it against the
release checksums and installs `/usr/local/bin/overload`.

## 2. Create the database

With a managed database, create a database named `overload` and copy its
connection string; managed databases need `?sslmode=require`.

On the same VM:

```sh
sudo apt-get install -y postgresql
sudo -u postgres psql -c "CREATE USER overload WITH PASSWORD 'choose-a-password';"
sudo -u postgres psql -c "CREATE DATABASE overload OWNER overload;"
```

The connection string is then
`postgres://overload:choose-a-password@127.0.0.1:5432/overload?sslmode=disable`.

## 3. Configure overload

Create a system user and write the configuration:

```sh
sudo useradd --system --home /var/lib/overload --create-home --shell /usr/sbin/nologin overload
sudo -u overload overload install --home /var/lib/overload --no-start \
  --database-url 'postgres://overload:…@…:5432/overload?sslmode=require'
```

This checks the database, creates overload's tables and writes
`/var/lib/overload/overload.env` (mode 600) with a generated UI user and
password. Add your model API key and turn on posting in the same file:

```sh
sudo -u overload tee -a /var/lib/overload/overload.env <<'EOF'
OVERLOAD_MODEL_OPENAI_KEY=sk-…
OVERLOAD_ENABLE_POSTING=1
EOF
```

Key names must not collide with overload's own secrets; a name starting with
`OVERLOAD_MODEL_` is always accepted.

## 4. Run it with systemd

```sh
sudo curl -fsSL -o /etc/systemd/system/overload.service \
  https://raw.githubusercontent.com/daltoniam/overload/main/deploy/systemd/overload.service
sudo systemctl daemon-reload
sudo systemctl enable --now overload
systemctl status overload
journalctl -u overload -f        # logs
```

The [unit](../deploy/systemd/overload.service) runs overload as the
`overload` user with a read-only system, private `/tmp` and no capabilities,
and restarts it if it stops. Overload listens on `127.0.0.1:8082` only.

## 5. Open the UI

The UI is not exposed to the internet. Reach it through SSH:

```sh
ssh -L 8082:127.0.0.1:8082 you@your-server
```

then open <http://127.0.0.1:8082> and sign in with `OVERLOAD_UI_USER` and
`OVERLOAD_UI_PASSWORD` from `overload.env`
(`sudo grep OVERLOAD_UI /var/lib/overload/overload.env`).

## 6. Expose the webhook

GitHub must reach `https://<hostname>/webhooks/github`. Expose only that
path. Pick one:

**Cloudflare Tunnel** (no open ports; the hostname's domain must be on
Cloudflare). Follow [cloudflare-tunnel.md](cloudflare-tunnel.md#by-hand-linux-or-a-custom-setup);
`sudo cloudflared service install` runs it under systemd.

**Caddy** (the hostname's DNS points at the VM; ports 80 and 443 open):

```sh
sudo apt-get install -y caddy
sudo tee /etc/caddy/Caddyfile <<'EOF'
overload.example.com {
	handle /webhooks/github {
		reverse_proxy 127.0.0.1:8082
	}
	handle {
		respond 404
	}
}
EOF
sudo systemctl reload caddy
```

Caddy obtains the TLS certificate. Check from your own machine:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://overload.example.com/webhooks/github   # 401
curl -s -o /dev/null -w '%{http_code}\n' https://overload.example.com/                         # 404
```

## 7. Add a model, a workflow and GitHub

In the UI (through the SSH tunnel):

1. **Models → New model**: connection type **Hosted API**, the provider's
   base URL (for example `https://api.openai.com/v1` or
   `https://openrouter.ai/api/v1`), the model ID and the key's variable name
   (`OVERLOAD_MODEL_OPENAI_KEY`). Set **Parallel files** to 8.
2. **Agents → New agent**: pick the model and **Start from: General code
   review**.
3. **Workflows → New workflow** with that agent as the main agent.
4. **GitHub**: create the App with `https://overload.example.com/webhooks/github`
   as the webhook URL, then install it on your repositories.
5. **Repositories**: enable a repository, check **Post reviews as GitHub
   comments**, and bind **Opened** and **Updated** to the workflow.

The [Mac guide](install-mac.md#3-create-agents-and-a-workflow) describes
these steps in more detail; they are the same on Linux.

## Upgrading

```sh
curl -fsSL https://raw.githubusercontent.com/daltoniam/overload/main/install.sh \
  | sudo OVERLOAD_NO_BREW=1 OVERLOAD_BINARY_ONLY=1 OVERLOAD_INSTALL_DIR=/usr/local/bin sh
sudo systemctl restart overload
```

Overload upgrades its database tables when it starts. Back up the database
before upgrading; it holds the GitHub App's private key, so treat backups
as secrets.

## Containers

Instead of the binary, run the image `ghcr.io/daltoniam/overload:<version>`
with the same variables (`DATABASE_URL`, `OVERLOAD_UI_USER`,
`OVERLOAD_UI_PASSWORD`, model keys) and `OVERLOAD_ADDR=0.0.0.0:8082`, and
publish port 8082 only to the reverse proxy.
