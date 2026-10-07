# overload

Overload reviews GitHub pull requests with AI models you control: a local
model on your own Mac (served on Metal by [DwarfStar](https://dwarfstar.sh/)
or llama.cpp) or any hosted OpenAI-compatible model. You decide which agents
review which files, every finding is checked against the diff before it is
shown, and reviews are posted back to the pull request as inline comments.

![Walkthrough: a workflow, its routing preview, a finished run and the comment posted on GitHub](docs/images/walkthrough.gif)

Status: pre-release. Local and webhook reviews, scheduled prompts, the
GitHub App and posting reviews to real pull requests work and are tested.

## Get started

| Where | Models | Guide |
| --- | --- | --- |
| Your Mac | Local or hosted | [docs/install-mac.md](docs/install-mac.md) |
| A Linux server (DigitalOcean, AWS, …) | Hosted | [docs/install-linux.md](docs/install-linux.md) |
| Kubernetes | Hosted | [docs/install-kubernetes.md](docs/install-kubernetes.md) |
| Receiving webhooks on a Mac or home server | | [docs/cloudflare-tunnel.md](docs/cloudflare-tunnel.md) |

On a Mac, the short version is:

```sh
curl -fsSL https://raw.githubusercontent.com/daltoniam/overload/main/install.sh | sh
```

This installs overload (and Postgres, with Homebrew), starts both as login
services and prints the UI address (`http://127.0.0.1:8082`) and password.

## How it works

**1. Agents and a workflow.** An agent is a model plus its instructions. A
workflow has a main agent and up to eight sub-agents, each limited to the
files it is for. An optional planner sends files to the right sub-agent
whatever their path, and an optional verifier drops weak findings before
they are posted.

![Workflow editor with a main agent, planner, verifier and three sub-agents](docs/images/workflow-editor.png)

**2. Preview the routing.** Paste a pull request's file list (or pick a real
pull request) to see who would review each file, how many model calls that
takes and roughly how long, before any model runs.

![Routing preview showing the model calls per agent and estimated duration](docs/images/preview.png)

**3. Reviews run on every pull request.** Bind a repository's pull request
events to the workflow. Each run records the findings, which agent reported
them, what the verifier dropped and how much each agent cost.

![A finished run with five findings ordered by severity](docs/images/run-findings.png)

![Per-agent breakdown of files, findings and tokens for a run](docs/images/run-agents.png)

**4. Findings are posted to GitHub.** With posting turned on, the GitHub App
leaves the review as inline comments on the changed lines.

![The overload GitHub App's inline comment flagging a SQL injection](docs/images/github-comment.png)

The dashboard tracks runs across all repositories:

![Overview dashboard with run counts and daily activity](docs/images/dashboard.png)

## Review a single pull request

```sh
overload review --repo owner/name --pr 123
```

This uses your default model and `gh auth token` (or `GITHUB_TOKEN`), prints
the findings and saves the run so you can open it in the UI. Nothing is
posted. `--workflow NAME` runs a saved workflow, and `--concurrency N`
reviews N files at once when the model server has parallel slots.

## Models

Overload talks to any OpenAI-compatible server. On a 64 GB M1 Max,
Qwen3.8 Flash Next on DwarfStar found all five bugs in our planted-bug test
file in four of four runs, at 8 to 12 minutes per file. See
[docs/install-mac.md](docs/install-mac.md#2-choose-a-model) for local and
hosted setups and [deploy/README.md](deploy/README.md#models) for
measurements.

## Configuration

Everything in the UI is also available from the CLI as JSON
(`overload agents|workflows|bindings|repositories|schedules list|apply`), so
a coding agent can configure overload too. [docs/workflows.md](docs/workflows.md)
describes every workflow setting.

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
