# Run overload on Kubernetes

This guide runs overload on a managed Kubernetes cluster (DigitalOcean
Kubernetes, Amazon EKS, Google GKE, …) with a managed Postgres database and
**hosted models**. Kubernetes suits a team that already runs a cluster; for
one server, [install-linux.md](install-linux.md) is simpler. Local models
belong on a Mac or a GPU machine at home, not in this setup.

Overload runs as one replica: reviews are queued in Postgres and run in
parallel inside the pod, and the UI's form tokens belong to one process, so
keep `replicas: 1`. The manifests are plain
Kustomize in [`deploy/k8s`](../deploy/k8s):

| Path | What it adds |
|---|---|
| `base` | Namespace, non-root Deployment with a read-only root filesystem, Service, ServiceAccount without an API token |
| `overlays/cloud` | A pinned image, an Ingress for the webhook path only, a NetworkPolicy |
| `overlays/cloudflare`, `components/postgres` | No Ingress or load balancer: Cloudflare Tunnel, Cloudflare Access for the dashboard, an in-cluster Postgres; see [cloudflare-kubernetes.md](cloudflare-kubernetes.md) |
| `overlays/kind`, `components/sandbox` | A self-contained test setup and sandboxed reviews (below) |

You need:

- A cluster with an ingress controller and cert-manager. The overlay
  assumes [ingress-nginx](https://kubernetes.github.io/ingress-nginx/) and a
  cert-manager `ClusterIssuer` named `letsencrypt`; DigitalOcean's
  marketplace installs both. On EKS with the AWS Load Balancer Controller,
  replace the Ingress (see below).
- Managed Postgres 14 or later with a database named `overload`.
- An API key for an OpenAI-compatible model provider.
- A hostname for webhooks whose DNS you can point at the ingress.

## 1. Create the secret

Everything overload reads from the environment comes from a Secret named
`overload`:

```sh
kubectl create namespace overload
kubectl -n overload create secret generic overload \
  --from-literal=DATABASE_URL='postgres://user:password@host:25060/overload?sslmode=require' \
  --from-literal=OVERLOAD_UI_USER=admin \
  --from-literal=OVERLOAD_UI_PASSWORD="$(openssl rand -hex 24)" \
  --from-literal=OVERLOAD_MODEL_OPENAI_KEY='sk-…' \
  --from-literal=OVERLOAD_ENABLE_POSTING=1
```

Add more `OVERLOAD_MODEL_*` keys for other providers. To change a value
later, update the secret and run
`kubectl -n overload rollout restart deployment/overload`.

## 2. Deploy

Copy `deploy/k8s/overlays/cloud` into your own repository (or reference it
with a remote base), then edit:

- `ingress.yaml`: your hostname (twice), and the issuer if yours has another
  name.
- `kustomization.yaml`: the image tag of the release you want.
- `networkpolicy.yaml`: the namespace of your ingress controller if it is
  not `ingress-nginx`.

```sh
kubectl apply -k overlays/cloud
kubectl -n overload rollout status deployment/overload
```

Overload creates its database tables on start. Point the hostname's DNS at
the ingress controller's load balancer, then check from your machine:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://overload.example.com/webhooks/github   # 401
curl -s -o /dev/null -w '%{http_code}\n' https://overload.example.com/                         # 404
```

Only the webhook is public. The UI stays inside the cluster.

### EKS with the AWS Load Balancer Controller

Replace `ingress.yaml` with an ALB Ingress that routes only the webhook:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: overload-webhook
  annotations:
    alb.ingress.kubernetes.io/scheme: internet-facing
    alb.ingress.kubernetes.io/target-type: ip
    alb.ingress.kubernetes.io/listen-ports: '[{"HTTPS":443}]'
    alb.ingress.kubernetes.io/certificate-arn: arn:aws:acm:…
spec:
  ingressClassName: alb
  rules:
    - host: overload.example.com
      http:
        paths:
          - path: /webhooks/github
            pathType: Exact
            backend:
              service:
                name: overload
                port:
                  name: http
```

and remove `networkpolicy.yaml` from the overlay (or allow traffic from the
VPC), since ALB targets pods directly.

## 3. Open the UI

```sh
kubectl -n overload port-forward svc/overload 8082
```

Open <http://127.0.0.1:8082> and sign in with `OVERLOAD_UI_USER` and
`OVERLOAD_UI_PASSWORD` from the secret. Then, as in the
[Linux guide](install-linux.md#7-add-a-model-a-workflow-and-github):

1. **Models**: a hosted connection with the provider's base URL, model ID
   and `OVERLOAD_MODEL_OPENAI_KEY` as the key variable; Parallel files 8.
2. **Agents** and a **Workflow**.
3. **GitHub**: create the App with `https://overload.example.com/webhooks/github`
   as its webhook URL and install it. The App's credentials are stored in
   Postgres; to manage them as secrets instead, set `GITHUB_APP_ID`,
   `GITHUB_APP_PRIVATE_KEY` and `GITHUB_WEBHOOK_SECRET` in the `overload`
   secret.
4. **Repositories**: enable the repository, allow posting and bind the PR
   actions to the workflow.

## Sandboxed reviews

`components/sandbox` runs each review in an isolated
[Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) pod. The
pull request is unpacked and read only inside that pod, which can reach
nothing but DNS, in-cluster model servers labeled `overload.dev/model:
"true"`, and overload's **model proxy** on port 8083.

API keys never enter a sandbox. For a model that needs a key, overload
gives the sandbox a short-lived proxy URL for that one model (valid for the
review); the proxy adds the key and forwards only chat completion and
Responses API requests naming that model. Set `OVERLOAD_MODEL_PROXY_URL`
(the component sets `http://overload:8083`) to enable it.

1. Install the Agent Sandbox controller (cluster-wide CRDs and a controller
   in `agent-sandbox-system`):

   ```sh
   kubectl apply --server-side -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox-with-extensions.yaml
   ```

2. Add `components/sandbox` to your overlay and set the agent image, as
   `overlays/cloudflare-sandbox` does:
   `ghcr.io/daltoniam/overload-agent:<version>`. The warm pool keeps idle
   sandbox pods ready (two by default; that overlay uses one).

3. Apply, then check that a sandbox pod is ready:
   `kubectl -n overload get sandboxes,pods -l app.kubernetes.io/name=overload-agent`.

Your network plugin must enforce NetworkPolicy (Cilium on DigitalOcean
does) or the sandbox is not isolated; `make kind-sandbox-test` probes the
same rules on kind.

## Upgrading

Change the image tag in `kustomization.yaml` and apply again. The
Deployment uses the `Recreate` strategy, so the old pod stops before the new
one migrates the database. Queued reviews wait in Postgres and continue.
