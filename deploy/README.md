# Running overload

Overload is built first for a single Mac with Apple Silicon: the app, its
Postgres database and the model all run locally. Linux servers and
Kubernetes are supported for teams; see the end of this page.

## Mac install

```sh
curl -fsSL https://raw.githubusercontent.com/daltoniam/overload/main/install.sh | sh
# or, with the binary already on PATH:
overload install
```

`install.sh` uses `brew install daltoniam/tap/overload` when Homebrew is
present (the formula depends on `postgresql@17`). Otherwise it downloads
`overload_<os>_<arch>.tar.gz` from the latest GitHub release, checks it
against `checksums.txt` and installs it into `~/.local/bin`
(`OVERLOAD_INSTALL_DIR`, `OVERLOAD_VERSION`, `OVERLOAD_NO_BREW=1` and
`OVERLOAD_INSTALL_ARGS` change this). It then runs `overload install`.

`overload install`:

- writes `~/Library/Application Support/overload/overload.env` (mode 600)
  with a generated UI password and database password. Every `overload`
  command reads that file, so `overload review` and `overload settings` work
  without exporting variables. Environment variables still win, and
  `OVERLOAD_CONFIG` points at another file.
- creates a private Postgres cluster in the install directory, listening only
  on 127.0.0.1 (port 55432 or the next free one). It uses Homebrew
  `postgresql@18` to `@14`, then Postgres.app; `--postgres-bin` picks another
  build and `--database-url` uses an existing server instead.
- runs migrations, registers launchd agents (`dev.overload` and
  `dev.overload.postgres`) that start at login and restart on failure, waits
  for `/healthz`, and prints the URL and password. If a model server answers
  on port 8000 (DwarfStar) or 8080 (llama.cpp) it prints the command to add
  it.

Running `install` again keeps the configuration and restarts the services.
`overload status` shows services and health. `overload uninstall` removes the
launchd agents and keeps the data; `--purge` deletes the install directory
too. `--home`, `--label` and `--port` allow side-by-side installs.

`make install-test` (and the macOS CI job) installs under a throwaway label
and port and checks login, `install-check`, a saved model surviving restarts
of both services, reinstall without new credentials, uninstall and reinstall
keeping data, and `--purge` cleanup.

The UI only answers to `127.0.0.1`, `localhost` and the host in
`OVERLOAD_BASE_URL`; set that to the address you use if it differs. This
blocks DNS-rebinding pages from reaching a local UI.

## Models

Any OpenAI-compatible `/v1/chat/completions` server works. Each model
connection has:

| Setting | CLI flag | Meaning |
|---|---|---|
| Connection | `--connection-kind local\|hosted` | Hosted models need `--api-key-env` |
| Reasoning control | `--reasoning-param` | How thinking is requested (below) |
| Reasoning effort | `--reasoning-effort` | none, minimal, low, medium, high, xhigh, max |
| Max output tokens | `--max-output-tokens` | Thinking counts against it; 0 means local 49,152, hosted 16,384 |
| Parallel files | `--concurrency` | Files reviewed at once, 1 to 32; match the server's parallel slots |

| Reasoning control | Sends | Use for |
|---|---|---|
| Automatic (default) | local: `chat_template_kwargs.reasoning_effort=xhigh`; hosted: nothing | llama.cpp models |
| `none` | nothing | Servers that reject reasoning fields |
| `chat_template` | `chat_template_kwargs.reasoning_effort=<effort>` | llama.cpp chat templates (Bonsai) |
| `reasoning_effort` | top-level `reasoning_effort=<effort>` | DwarfStar `ds4-server`, OpenAI-style APIs |

If a call runs out of tokens while thinking, that file is retried once at
`medium` and the run records `effort_fallbacks`. Settings are pinned into
each run, so editing a model does not change queued reviews. Only the name
of an API key's environment variable is stored, never the key, and names of
overload's own secrets (`DATABASE_URL`, `OVERLOAD_*`, `GITHUB_*`, `PG*`) are
refused.

### DwarfStar on Metal

[DwarfStar](https://dwarfstar.sh/) (`github.com/antirez/ds4`, MIT) serves a
few large mixture-of-experts models (DeepSeek V4/V4.1 Flash, GLM 5.x, Qwen3.8
Flash Next) on Apple Metal.

```sh
git clone https://github.com/antirez/ds4 && cd ds4 && make
./download_model.sh qwen38-q2          # needs the hf CLI; 137 GiB on disk
./ds4-server --ctx 32768 --prefill-chunk 1024 --port 8000
overload settings set --name ds4 --url http://127.0.0.1:8000/v1 \
  --model qwen3.8-flash-next --reasoning-param reasoning_effort \
  --reasoning-effort high --max-output-tokens 28000 --default
```

Qwen3.8 Q2 uses about 44 GiB of memory at 32k context and fits a 64 GB Mac;
DeepSeek V4 Flash Q2 needs 96 to 128 GB. `ds4-server` handles one request at
a time unless started with `--batched-session N`, so keep Parallel files at 1
otherwise.

### llama.cpp

Start `llama-server --jinja` with a context of at least 64k tokens per slot
(the largest review prompt is about 11k tokens plus up to 49k of output) and
add it with `--reasoning-param chat_template`. A per-request effort overrides
a server default such as `--chat-template-kwargs '{"reasoning_effort":"medium"}'`.

### Measurements

A 92-line Go file with five planted bugs (`harness/testdata/planted`), run
through the normal review path on an M1 Max with 64 GB. Rerun with
`OVERLOAD_PLANTED_EVAL=1 go test -run TestPlantedBugEval -v ./harness/` and
the `OVERLOAD_EVAL_*` variables described in the test.

| Model | Planted bugs found | Output tokens | Time per file |
|---|---|---|---|
| Qwen3.8 Flash Next Q2, ds4, `reasoning_effort:high` | 5/5 in 4 of 4 runs | 7.5k to 13.5k | 8 to 12 min |
| Bonsai 2 27B, llama.cpp, `xhigh` | 4/5 in 2 of 2 runs | 11k to 15k | 15 to 19 min |
| Bonsai 2 27B, llama.cpp, `medium` | 3/5 in 2 of 2 runs | 2.7k to 3.5k | 3.5 to 4.5 min |
| GPT-6 Sol (hosted) | 5/5 in 14 of 14 runs | 1.4k to 2.0k | 13 to 21 s |

GPT-6 Sol counts its hidden reasoning against the output limit: with the
old 2,048-token hosted default, two runs were cut off and lost most of their
findings, so the default is now 16,384 (you pay only for tokens used). Two
correct findings it worded or anchored differently were scored as misses
until the scorer was widened. No run reported a false positive. Bonsai missed the mutex left locked on a
cache hit at both efforts. This is one synthetic file; treat it as
directional. Parallel files did not help on the M1 Max (two streams gave
10 tokens/s combined against 13 for one); it is meant for GPUs and hosted
APIs, where eight at once cut a 21-file review from 138 s to 24 s.

Local connections have no response-header timeout, because a non-streaming
reply sends nothing until all thinking is done; the review timeout (260
minutes by default) still bounds every call.

## GitHub

### Pull requests without an App

`overload review --repo owner/name --pr N` uses `gh auth token`,
`GITHUB_TOKEN` or `GH_TOKEN`. For webhooks without an App, add the
repository on the Repositories page, set `GITHUB_WEBHOOK_SECRET` and point
a repository webhook at `/webhooks/github`.

### GitHub App

**GitHub** in the UI (`/setup/github`) asks for an App name, optional
organization and a public `https://` webhook URL. A Mac needs a tunnel to
port 8082, for example Cloudflare Tunnel or Tailscale Funnel; expose only
`/webhooks/github`, since webhooks are signed but the UI uses Basic Auth.

With Cloudflare Tunnel (`brew install cloudflared`, then
`cloudflared tunnel login` once to choose the domain):

```sh
cloudflared tunnel create overload
cloudflared tunnel route dns overload overload.example.com
```

`~/.cloudflared/config.yml` forwards only the webhook path; everything else,
including the UI, gets a 404 at Cloudflare and never reaches the Mac:

```yaml
tunnel: overload
credentials-file: /Users/you/.cloudflared/<tunnel-id>.json
ingress:
  - hostname: overload.example.com
    path: ^/webhooks/github$
    service: http://127.0.0.1:8082
  - service: http_status:404
```

`cloudflared service install` runs it at login. Use
`https://overload.example.com/webhooks/github` as the webhook URL, and keep
using `http://127.0.0.1:8082` for the UI.

Overload sends you to GitHub with a manifest and a one-time state value, and
GitHub returns a code that overload exchanges for the App ID, private key
and webhook secret. These are stored in Postgres (table `github_app`), so
protect database backups. `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY[_PATH]`
and `GITHUB_WEBHOOK_SECRET` in the environment take precedence.

The App asks for contents read, metadata read and pull requests write.
Installation webhooks keep repositories in sync. Repositories are matched by
GitHub's repository ID, so a renamed repository keeps its settings and a
different repository that later takes the old name does not inherit them.
New repositories arrive disabled and dry-run. A pull request event must come
from the installation its repository is linked to; each run pins that
installation and uses its short-lived tokens. Repositories added by hand use
the configured token.

Tested with fakes for GitHub (manifest exchange, signatures, installation
sync, installation tokens); not yet run against a real App.

### Posting reviews

Posting needs all three: `OVERLOAD_ENABLE_POSTING=1`, **Post reviews as
GitHub comments** checked on the repository, and the repository covered by
the App. Otherwise findings stay dry-run and the run records why
(`posting_disabled`, `no_github_app_installation`).

Posting is a separate job queued in the same transaction that completes the
review. Each review carries a hidden `<!-- overload-run:N -->` marker, so a
retried job finds the review it already posted. A finding already posted on
the pull request is not posted again, even if its line moved. Model text is
stripped of links, images, HTML and @mentions before it is stored, because
the model reads untrusted pull request content.

## Linux servers

`overload install` on Linux writes the configuration and database (with
`--postgres-bin` or `--database-url`) and prints the `overload serve` command;
it does not manage services. Run that under systemd or a container. The app
image is built from `deploy/images/app/Dockerfile` and published to
`ghcr.io/daltoniam/overload` on release tags.

## Kubernetes

Tested on disposable kind clusters (kind v0.33, Agent Sandbox v1.0.4); not
yet run on a managed provider.

- `deploy/k8s/base`: namespace, non-root app Deployment (read-only root,
  `/tmp` emptyDir, probes on `/healthz`), Service and ServiceAccount. It
  expects a Secret named `overload` with `DATABASE_URL`, `OVERLOAD_UI_USER`
  and `OVERLOAD_UI_PASSWORD`; bring your own Postgres.
- `deploy/k8s/overlays/kind`: base plus a single-replica Postgres
  StatefulSet and a network policy that lets only the app reach it.
- `deploy/k8s/components/sandbox` and `overlays/kind-sandbox`: run webhook
  reviews in Agent Sandbox pods (`OVERLOAD_SANDBOX=agent-sandbox`), with a
  namespaced Role for sandbox claims and a warm pool. Needs the Agent Sandbox
  controller and the `overload-agent` image (`deploy/images/agent`).
- `make kind-test` creates a cluster with its own kubeconfig file, installs
  the kind overlay and checks health, login, `install-check` and data
  surviving Postgres and app restarts. It never touches your current kubectl
  context.
- `make kind-sandbox-test` installs Agent Sandbox and a fake in-cluster
  model, sends a signed webhook for a public pull request (`OVERLOAD_KIND_PR`),
  and checks the review ran in a sandbox, the model received the diff, and
  the sandbox was deleted. Needs `gh auth` or `GITHUB_TOKEN`.

In sandbox mode the app downloads the pull request but never unpacks it; the
archive is extracted and reviewed inside the sandbox and only `result.json`
(at most 8 MiB) comes back, to be re-validated against the diff. Models that
need an API key are refused so credentials never enter sandbox pods.
Sandbox pods accept connections only from the app (ports 8080 and 9090) and
may reach only cluster DNS and pods labeled `overload.dev/model: "true"`;
the template sets its own network policy because the controller's default
allows the whole internet, and `dnsPolicy: ClusterFirst` because the default
points DNS at public resolvers. Whether the Kubernetes API is blocked depends
on the network plugin (kind v0.31 allowed it), so check new clusters with the
same probe the test uses.

## Releases

On a `v*` tag, GoReleaser builds darwin and linux archives for amd64 and
arm64 (`overload_<os>_<arch>.tar.gz`, unversioned names so `install.sh` can
use `releases/latest/download`) and `checksums.txt`, publishes the GitHub
release, and updates `Formula/overload.rb` in `daltoniam/homebrew-tap` using
the `RELEASE_TOKEN` secret. The same workflow pushes multi-arch app images to
GHCR. `make release-snapshot` builds the archives locally. macOS binaries are
ad-hoc signed by the Go linker, which is enough for curl and Homebrew
installs; a downloadable `.pkg` would need Developer ID signing.

## Release gates

- [x] Mac install, restart and reinstall test in CI.
- [x] Kubernetes install and sandboxed review on a disposable kind cluster.
- [ ] GitHub App and repository sync against a real App and repository.
- [ ] Posting to a real pull request.
- [ ] A published release and Homebrew formula, installed on a clean Mac.
- [ ] A reaper for sandbox claims left by a crashed worker.
- [ ] Backup, restore and upgrade tests.
