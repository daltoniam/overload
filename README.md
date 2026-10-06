# overload

Overload reviews GitHub pull requests with AI models you run on your own Mac.
It is built for Apple Silicon: a local model served on Metal (by
[DwarfStar](https://dwarfstar.sh/) or llama.cpp) reads each changed file,
and overload checks every finding against the diff before it is shown or
posted. Hosted OpenAI-compatible models work too.

Status: pre-release. Local and webhook reviews, scheduled prompts, the GitHub
App setup and opt-in posting work and are tested; the GitHub App and posting
have only been exercised against fakes so far.

## Install on a Mac

```sh
curl -fsSL https://raw.githubusercontent.com/daltoniam/overload/main/install.sh | sh
```

With Homebrew this runs `brew install daltoniam/tap/overload` (which brings
Postgres 17); otherwise it downloads the release binary into `~/.local/bin`.
It then runs `overload install`, which:

- writes `~/Library/Application Support/overload/overload.env` (readable only
  by you) with a generated UI password;
- creates a private Postgres database in the same directory;
- registers login services for Postgres and overload that restart on failure;
- prints the UI address (`http://127.0.0.1:8082`) and password, and suggests
  a model if one is already running on port 8000 or 8080.

`overload status` shows what is running, `overload uninstall` stops it and
keeps your data, and `overload uninstall --purge` removes everything.

## Run a model

Overload talks to any OpenAI-compatible server. On a Mac, DwarfStar runs
large mixture-of-experts models on Metal:

```sh
git clone https://github.com/antirez/ds4 && cd ds4 && make
./download_model.sh qwen38-q2          # 137 GiB on disk, ~42 GiB in memory
./ds4-server --ctx 32768 --prefill-chunk 1024 --port 8000

overload settings set --name ds4 --url http://127.0.0.1:8000/v1 \
  --model qwen3.8-flash-next --reasoning-param reasoning_effort \
  --reasoning-effort high --max-output-tokens 28000 --default
```

On a 64 GB M1 Max, Qwen3.8 Flash Next found all five bugs in our planted-bug
test file in four of four runs, at 8 to 12 minutes per file. DeepSeek V4 Flash
needs 96 to 128 GB. llama.cpp models (for example Bonsai on
`llama-server`) use `--reasoning-param chat_template`. See
[deploy/README.md](deploy/README.md#models) for settings, measurements and
hosted models.

## Review a pull request

```sh
overload review --repo owner/name --pr 123
```

This uses your default model and `gh auth token` (or `GITHUB_TOKEN`), prints
the findings and saves the run, which you can open in the UI. Nothing is
posted to GitHub. `--workflow NAME` runs a saved multi-agent workflow instead,
and `--concurrency N` reviews N files at once when the model server has
parallel slots.

## Review every pull request

1. Open **GitHub** in the UI and create a GitHub App. GitHub needs a public
   `https://` URL for webhooks; for a Mac, point a tunnel (Cloudflare Tunnel,
   Tailscale Funnel) at port 8082 and use `https://<host>/webhooks/github`.
2. Install the App on your repositories. They appear under **Repositories**,
   disabled and dry-run.
3. Create **Agents** (a model and its instructions) and a PR **Workflow**,
   then enable a
   repository and bind its PR actions to the workflow. A workflow has a
   main agent and up to eight sub-agents, each limited to the files it is
   for; **Preview routing** on the workflow shows who would review what
   in a real pull request before any model runs.
4. To post reviews as comments, check **Post reviews as GitHub comments** on
   the repository and set `OVERLOAD_ENABLE_POSTING=1` in `overload.env`.

Every setting is also available from the CLI (`overload agents|workflows|
bindings|repositories|schedules list|apply`), so an agent can configure
overload without the UI. Agent JSON holds the instructions directly:
`{"name": "security", "model": "qwen", "prompt": "...", "enabled": true}`. Scheduled prompts run workflows on a cron
schedule with JSON input.

A PR workflow's first agent is its main agent; the rest are sub-agents. In
the workflow JSON, `skip_paths` drops files such as `*.lock` or
`vendor/**`, `scopes` limits a sub-agent to matching files (and optionally
its number of findings), `main_reviews: "unclaimed"` leaves files a
sub-agent claimed to that sub-agent, and `max_file_reviews` caps the work per
run. A scope's `mode` is `globs` (default), `always` (only its paths) or
`planned` (only files the planner assigns), and its `description` tells the
planner what the sub-agent is for. `planner_prompt` holds instructions that
let the main agent's model send files to sub-agents whatever they are called;
`verifier_prompt` holds instructions that keep or drop each sub-agent finding
before posting. Changed instructions are saved as new versions; each run
records the exact text it used. `overload workflows preview NAME <
files.txt` shows which agent would review each file and roughly how long it
would take, without calling a model.

## Other deployments

Linux servers and Kubernetes (with reviews in isolated Agent Sandbox pods)
are supported and tested on kind; see [deploy/README.md](deploy/README.md).

## Development

Requires Go 1.26.5+ and Postgres (`overload install` or
`docker compose up -d postgres`).

```sh
export DATABASE_URL=postgres://overload:overload@localhost:5432/overload?sslmode=disable
make ci                 # generate, gofmt, vet, lint, test, build
make install-test       # macOS: install under a test label and check it end to end
make kind-test          # disposable kind cluster: app, Postgres, restarts
```

`OVERLOAD_DEV_SEED=1 overload dev-seed` adds disabled example configuration
and one sample run for exploring the UI. [PLAN.md](PLAN.md) holds the design
and roadmap; [AGENTS.md](AGENTS.md) the conventions for coding agents.
