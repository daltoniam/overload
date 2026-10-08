# Kubernetes behind Cloudflare Tunnel and Access

This guide runs overload on any Kubernetes cluster with no load balancer, no
public IP and no Ingress:

- **Cloudflare Tunnel**: `cloudflared` pods in the cluster connect out to
  Cloudflare, and Cloudflare sends requests for one hostname back through
  them.
- **Cloudflare Access**: you sign in to the dashboard with your email or
  identity provider. Overload checks Cloudflare's signed token on every
  request, so the dashboard cannot be reached around Access, even from
  inside the cluster.
- **Webhooks**: `/webhooks/github` skips Access. GitHub signs every
  delivery, and overload rejects unsigned ones.

```
GitHub ──webhook──┐
                  ├─► Cloudflare ─► tunnel ─► cloudflared pods ─► overload ─► Postgres
You ─► Access ────┘                                                   └─► hosted models
```

The setup is plain Kustomize in
[`deploy/k8s/overlays/cloudflare`](../deploy/k8s/overlays/cloudflare). It
includes a small Postgres StatefulSet; to use a managed database instead,
see [step 4](#4-create-the-secrets). Reviews use **hosted models** (OpenAI,
OpenRouter, Cloudflare AI Gateway, …); local models belong on a Mac or GPU
machine.

You need:

- A Kubernetes cluster and `kubectl`. Any provider works; this was tested
  on DigitalOcean Kubernetes.
- A domain on Cloudflare (the free plan is enough), with Zero Trust turned
  on. Zero Trust is free for up to 50 users. The first time you open
  **Zero Trust** in the Cloudflare dashboard, it asks for a team name.
- An API key for an OpenAI-compatible model provider.
- Overload 0.1.1 or later. Earlier versions only support the UI password,
  not Access.

The examples use `overload.example.com`; replace it with your hostname.

## 1. Create the tunnel

**In the dashboard** (easiest):

1. Go to **Zero Trust → Networks → Tunnels → Create a tunnel**, choose
   **Cloudflared**, and name it `overload`.
2. On the install step, copy the token: the long value after
   `--token` in any of the install commands. You don't need to run the
   commands.
3. Add a **public hostname**: subdomain `overload`, your domain, service
   type **HTTP**, URL `overload.overload.svc.cluster.local:8082`.

**Or with the CLI**:

```sh
cloudflared tunnel login
cloudflared tunnel create overload
cloudflared tunnel route dns <TUNNEL-ID> overload.example.com
cloudflared tunnel token <TUNNEL-ID>
```

Pass the tunnel ID, not its name, and check the `tunnelID` that `route dns`
prints. If `~/.cloudflared/config.yml` exists for another tunnel,
`route dns` can attach the hostname to that tunnel instead. If that happens,
run it again with `--overwrite-dns`.

A tunnel created with the CLI sends every request to overload through the
ConfigMap in `cloudflared.yaml`. A tunnel created in the dashboard uses the
public hostname you set there.

## 2. Protect the dashboard with Access

In **Zero Trust → Access → Applications**, add two **self-hosted**
applications on the same hostname. Access applies the most specific path
first.

| Application | Domain and path | Policy |
| --- | --- | --- |
| `overload` | `overload.example.com` (no path) | **Allow**: the emails or groups that may use overload |
| `overload webhooks` | `overload.example.com` / `webhooks/github` | **Bypass**: Everyone |

Then copy two values for overload:

- The **Application Audience (AUD) tag** of the `overload` application
  (on its **Overview** tab), for `OVERLOAD_ACCESS_AUD`.
- Your **team domain**, `<team>.cloudflareaccess.com` (under **Settings →
  Custom pages**, or in the browser's address bar when you sign in), for
  `OVERLOAD_ACCESS_TEAM_DOMAIN`.

## 3. Get the manifests

Copy `deploy/k8s/overlays/cloudflare` into your own repository, or
reference it as a remote base, and set the image tag in
`kustomization.yaml` to the release you want.

## 4. Create the secrets

```sh
kubectl create namespace overload

DB_PASSWORD="$(openssl rand -hex 24)"
kubectl -n overload create secret generic overload-postgres \
  --from-literal=password="$DB_PASSWORD"

kubectl -n overload create secret generic cloudflared \
  --from-literal=token='<TUNNEL TOKEN>'

kubectl -n overload create secret generic overload \
  --from-literal=DATABASE_URL="postgres://overload:$DB_PASSWORD@postgres:5432/overload?sslmode=disable" \
  --from-literal=OVERLOAD_BASE_URL=https://overload.example.com \
  --from-literal=OVERLOAD_ACCESS_TEAM_DOMAIN=<team>.cloudflareaccess.com \
  --from-literal=OVERLOAD_ACCESS_AUD='<AUD TAG>' \
  --from-literal=OVERLOAD_MODEL_API_KEY='<MODEL API KEY>' \
  --from-literal=OVERLOAD_ENABLE_POSTING=1
```

- With the Access variables set, overload doesn't ask for a UI password.
  Without them, it needs `OVERLOAD_UI_USER` and `OVERLOAD_UI_PASSWORD`, and
  your browser asks for them after Access.
- `OVERLOAD_BASE_URL` must be the public address. Overload rejects requests
  for other host names, and the GitHub App's webhook URL is built from it.
- Model API keys go in variables that start with `OVERLOAD_MODEL_`. Each
  model setting names the variable that holds its key.
- **Managed Postgres instead of the StatefulSet:** remove the
  `components/postgres` line from `kustomization.yaml`, skip the
  `overload-postgres` secret, and set `DATABASE_URL` to the managed
  database (usually with `sslmode=require`).

## 5. Deploy and check

```sh
kubectl apply -k overlays/cloudflare
kubectl -n overload rollout status deployment/overload
kubectl -n overload logs deployment/cloudflared | grep Registered
```

The overload pod may restart once while Postgres starts. From any machine:

```sh
curl -s https://overload.example.com/healthz                                       # ok
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://overload.example.com/webhooks/github   # 401 (unsigned)
curl -s -o /dev/null -w '%{http_code}\n' https://overload.example.com/              # 302 to Access sign-in
```

Then open `https://overload.example.com` in a browser and sign in through
Access.

## 6. Add a model, GitHub and a workflow

In the dashboard:

1. **Models**: add a hosted connection with the provider's base URL, model
   ID, and `OVERLOAD_MODEL_API_KEY` as the key variable. For example, for
   [Cloudflare AI Gateway](https://developers.cloudflare.com/ai-gateway/)'s
   OpenAI-compatible endpoint:
   - Base URL: `https://gateway.ai.cloudflare.com/v1/<ACCOUNT>/<GATEWAY>/compat`
   - Model: `workers-ai/@cf/moonshotai/kimi-k2.7-code`, or a provider
     model such as `openai/gpt-5.2`
2. **GitHub**: create the GitHub App and install it on your repositories.
   The webhook URL is filled in from `OVERLOAD_BASE_URL`.
   - A private App can only be installed on the account that owns it. To
     review repositories of several accounts (for example your user and an
     organization), make the App public afterwards: in its GitHub settings,
     **Advanced → Make public**. Then install it on each account.
   - Repositories from unknown installations arrive disabled, so a stranger
     installing a public App costs nothing.
3. **Agents** and a **Workflow**, then **Repositories**: enable each
   repository, allow posting, and bind its pull request actions to the
   workflow. See [workflows.md](workflows.md).

## Managing overload from the command line

Every setting is also available from the `overload` CLI inside the pod:

```sh
kubectl -n overload exec deploy/overload -- overload settings list
kubectl -n overload exec -i deploy/overload -- overload workflows apply - < workflow.json
gh pr view 123 --json files -q '.files[].path' |
  kubectl -n overload exec -i deploy/overload -- overload workflows preview my-workflow -
```

## Upgrading

Change the image tag in `kustomization.yaml` and apply again. The
Deployment uses the `Recreate` strategy, so the old pod stops before the new
one migrates the database. Queued reviews wait in Postgres and continue.
cloudflared keeps the tunnel up during the restart, and GitHub retries
webhooks that fail.
