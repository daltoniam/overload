# overload: Plan

## 1. Goal

overload reviews GitHub pull requests with AI models you run yourself, and
runs other single-shot agent jobs (scheduled prompts today). The primary
target is **one Mac with Apple Silicon**: the app, Postgres and a local model
served on Metal by [DwarfStar](https://dwarfstar.sh/) or llama.cpp, installed
with one command. Hosted OpenAI-compatible models are supported for speed,
and Linux servers and Kubernetes (with Agent Sandbox isolation) serve teams.

Each job is: event in, one isolated agent run, result out. There is no chat
bridge and no multi-turn conversation.

### Goals for v1

- One-command Mac install (`install.sh`, Homebrew formula, `overload install`
  with a private Postgres and launchd services).
- First-class local models on Metal: DwarfStar ds4 and llama.cpp, with
  per-model reasoning control, output budgets and parallel file reviews.
- A GitHub App (created from a manifest in the UI) receives pull request
  webhooks; reviews stay dry-run until a repository opts in to posting.
- Every finding is validated against the diff before it is stored or posted.
- Postgres is the only stateful dependency; River provides the job queue.
- A small server-rendered UI (templ) and an equivalent CLI for agents.
- A built-in evaluation path that measures review quality per model.

### Non-goals for v1

- Chat integrations and multi-turn agents.
- Multi-tenant SaaS concerns (orgs, billing, SSO).
- Executing PR code (tests, builds).

## 2. Decisions

| Area | Decision |
|---|---|
| Language | Go 1.26 |
| Module | `github.com/daltoniam/overload`, Apache-2.0 |
| Primary platform | macOS on Apple Silicon; models on Metal |
| Storage | Postgres 14+ (Homebrew `postgresql@17` by default), `jackc/pgx/v5` |
| Queue | River (`riverqueue/river`, `riverpgxv5`), inserted in the same transaction as domain rows |
| Migrations | Embedded SQL files for app tables; River's own migrations via `rivermigrate` |
| UI | `a-h/templ`, server-rendered (`layouts/`, `components/`, `pages/`), minimal JS |
| Agent harness | `charm.land/fantasy` with the `openaicompat` provider |
| Local models | DwarfStar `ds4-server` (port 8000) and llama.cpp `llama-server` (port 8080) |
| Install | `install.sh`, Homebrew tap `daltoniam/tap`, `overload install` (launchd) |
| Releases | GoReleaser: macOS/Linux archives, Homebrew formula, GHCR images |
| Isolation (teams) | Agent Sandbox v1.0.4 on Kubernetes: SandboxTemplate + SandboxWarmPool + SandboxClaim |
| GitHub | GitHub App from a manifest; repositories keyed by GitHub repository ID; per-repository posting opt-in |
| Binaries | `overload` (server, worker, UI, CLI, installer) and `overload-agent` (runs inside a sandbox) |

## 3. Architecture

```
GitHub ──webhook──> overload server ──(one tx)──> Postgres
                     │                           ├─ webhook_deliveries
                     │                           ├─ runs
                     │                           └─ river_job
                     │
                     └─ templ UI (runs, findings, repos, models, evals)

overload worker (River) ──> ReviewPRJob
   1. mint GitHub installation token (control plane only)
   2. fetch PR metadata, diff, head tarball
   3. claim sandbox from warm pool (Agent Sandbox SDK)
   4. upload tarball + spec.json, run `overload-agent review`
   5. read result.json, delete sandbox
   6. validate / dedupe / filter findings
   7. post GitHub review (or dry run), store findings

sandbox pod (overload-agent image)
   overload-agent ──OpenAI-compatible HTTP──> model endpoint
   (read-only tools over the extracted repo; no GitHub credentials)
```

Key properties:

- **Transactional ingest.** The webhook handler inserts the delivery, the run
  and the River job in one transaction. Either all exist or none do.
  Duplicate deliveries are rejected by a unique `delivery_id`.
- **Credentials stay in the control plane.** The worker downloads the head
  commit tarball (`GET /repos/{owner}/{repo}/tarball/{sha}`) and streams it
  into the sandbox with `Sandbox.WriteReader`. The sandbox never receives a
  GitHub token. Only the worker posts reviews.
- **The sandbox is treated as hostile.** PR content is untrusted input and
  may contain prompt injection. The harness has only read-only, path-confined
  tools; its output is schema-validated and sanitized by the worker before
  anything reaches GitHub.
- **One run = one sandbox.** River's worker count for the `reviews` queue is
  the concurrency limit (default 2).

## 4. Components

### 4.1 `overload` binary (`cmd/overload`)

Subcommands:

- `overload serve`: HTTP server (webhooks, UI, health). Also runs River workers
  unless `--no-worker` is set, so one process is enough locally.
- `overload worker`: River workers only (for scaling separately later).
- `overload migrate`: apply app and River migrations.
- `overload review --repo owner/name --pr N [--profile NAME] [--mode MODE] [--post]`:
  enqueue (or run inline with `--inline`) a review without a webhook. Dry run
  unless `--post`. This is the main tool for early development.
- `overload eval run --set NAME [--profiles a,b] [--modes context,plan,tools]`:
  run an evaluation matrix (section 9).

Configuration via environment variables (flags override), loaded into one
`Config` struct and validated at startup:

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | Postgres DSN |
| `OVERLOAD_ADDR` | Listen address, default `:8080` |
| `OVERLOAD_BASE_URL` | External URL, used for links in review comments |
| `OVERLOAD_UI_USER`, `OVERLOAD_UI_PASSWORD` | Basic auth for the UI (required unless Cloudflare Access or `OVERLOAD_UI_INSECURE=1`) |
| `OVERLOAD_WEBHOOK_CATCHUP` | `0` turns off redelivering GitHub webhooks that never reached overload (on by default with a GitHub App) |
| `OVERLOAD_TOOL_*` | Bearer tokens for tool servers (Switchboard API keys), named by each tool server |
| `OVERLOAD_ACCESS_TEAM_DOMAIN`, `OVERLOAD_ACCESS_AUD` | Cloudflare Access instead of basic auth: every UI request needs a valid `Cf-Access-Jwt-Assertion` for that application (RS256, team keys fetched hourly and on rotation) |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY` or `GITHUB_APP_PRIVATE_KEY_PATH`, `GITHUB_WEBHOOK_SECRET` | GitHub App |
| `OVERLOAD_SANDBOX_NAMESPACE`, `OVERLOAD_SANDBOX_WARMPOOL` | Agent Sandbox target |
| `OVERLOAD_SANDBOX_CONNECTIVITY` | `port-forward` (dev, overload on host) or `in-cluster-service` |
| `OVERLOAD_MAX_CONCURRENT_RUNS` | River worker count for `reviews`, default 2 |
| `OVERLOAD_RUN_TIMEOUT` | Hard timeout per run, default 15m |
| `OVERLOAD_DEFAULT_MODEL_BASE_URL`, `OVERLOAD_DEFAULT_MODEL`, `OVERLOAD_DEFAULT_MODEL_API_KEY` | Seed the default model profile on first start |

### 4.2 `overload-agent` binary (`cmd/overload-agent`)

Runs inside the sandbox. It is a normal CLI with no server of its own:

```
overload-agent review --spec /work/spec.json --repo /work/repo --out /work/result.json
```

- Reads `spec.json`: PR metadata, parsed diff, context budget, model
  settings (base URL, model name, temperature, max tokens, context window),
  mode, step limit, repository review instructions.
- Writes `result.json`: findings, summary, metrics (tokens in/out, steps,
  tool calls, durations), a transcript (for the UI) and any error.
- Exits 0 whenever it produced a valid `result.json`, even if the model
  failed (the error is recorded inside the result). Non-zero means an
  infrastructure failure the worker may retry.
- Must be able to run outside a sandbox against a local checkout
  (`--repo ~/code/somerepo`). This makes prompt iteration fast.

### 4.3 Sandbox image (`deploy/images/agent/Dockerfile`)

Contains `overload-agent`, `ripgrep`, `git`, `tar` and the Agent Sandbox
runtime server the SDK talks to. **Spike required (M2):** confirm which
runtime v1.0.4 expects in custom images (`sandboxd` vs `legacy-python`,
see `Runtime` in the SDK's `options.go`) and how to build on top of it.
Run as non-root with a read-only root filesystem and a writable `/work`
`emptyDir`.

## 5. Package layout

Root package `overload` holds domain types and interfaces only. Implementations
live in subpackages named after their dependency.

```
overload/
  overload.go            domain types: Run, RunStatus, Finding, Repository, ModelProfile, ReviewSpec, ReviewResult
  store.go             Store interfaces (RunStore, FindingStore, RepoStore, ...)
  sandbox.go           SandboxRunner interface
  harness.go           Harness interface (see 6.2)
  cmd/overload/          main, subcommands, config
  cmd/overload-agent/    in-sandbox CLI
  postgres/            Store implementations (pgx), migrations (embed), tx helpers
  queue/               River client setup, job args, workers (ReviewPRJob, ReapSandboxesJob, EvalCaseJob)
  github/              App auth, installation tokens, webhook verification/parsing, PR fetch, review posting
  diff/                unified diff parsing, hunk/line mapping, commentable-line checks
  review/              pipeline orchestration: build spec, validate, dedupe, filter, render comments
  harness/             Fantasy-based reviewer: prompts, modes, tools, output parsing
  harness/tools/       read_file, search, list_dir, get_diff (path-confined)
  sandbox/             Agent Sandbox implementation of SandboxRunner + local exec fake
  eval/                eval sets, scoring, reports
  catalog/             model catalog (provenance, licences, settings, evals) and the model policy
  localmodels/         verified model downloads, disk accounting, llama.cpp build checks, llama-server services
  http/                router, middleware (auth, CSRF, logging), webhook + UI handlers
  web/templates/       templ: layouts/, components/, pages/ (Switchboard pattern)
  web/static/          CSS (embedded)
  deploy/kind/         kind config + setup scripts
  deploy/k8s/          kustomize base + overlays (dev, prod): overload, postgres (dev only), llama.cpp, agent-sandbox resources, NetworkPolicies
  deploy/images/       Dockerfiles for overload and overload-agent
  testdata/            diffs, recorded model responses, fixture repos
```

## 6. Core interfaces

Keep these small so implementations can be swapped and faked in tests.

### 6.1 Sandbox runner

```go
type SandboxRunner interface {
    // Start claims a sandbox and returns a handle. Callers must call Close.
    Start(ctx context.Context, opts SandboxOptions) (Sandbox, error)
}

type Sandbox interface {
    Name() string
    Upload(ctx context.Context, name string, r io.Reader) error
    Exec(ctx context.Context, command string) (ExecResult, error)
    Download(ctx context.Context, name string, w io.Writer) (int64, error)
    Close(ctx context.Context) error // deletes the claim; idempotent
}
```

Implementations:

- `sandbox.AgentSandbox`: wraps `sandbox.NewClient` / `CreateSandbox(ctx, warmPoolName, namespace)` /
  `Run` / `WriteReader` / `ReadTo` / `DeleteSandbox`. Note the SDK only
  accepts plain file names for writes (no directories), so upload
  `repo.tar.gz` and `spec.json`, then extract with a command.
- `sandbox.Local`: runs `overload-agent` as a local subprocess in a temp dir.
  Used for tests, `overload review --inline --local` and developing without a
  cluster.
- A plain Kubernetes `Job` implementation is the fallback if Agent Sandbox
  becomes a blocker. Do not build it unless needed.

### 6.2 Harness

The worker only knows it runs a command in a sandbox and receives a
`ReviewResult`. Inside `overload-agent`, the reviewer is behind:

```go
type Reviewer interface {
    Review(ctx context.Context, spec ReviewSpec, repo fs.FS) (ReviewResult, error)
}
```

v1 has one implementation (Fantasy) with three modes. A CLI harness
(section 13) becomes a second implementation later that shells out and
converts output into the same `ReviewResult`.

## 7. Review pipeline

### 7.1 Triggers

- GitHub `pull_request` actions: `opened`, `synchronize`, `reopened`,
  `ready_for_review`. Skip drafts (configurable per repo), skip bot authors,
  skip repositories that are not enabled.
- GitHub `issue_comment` with `/overload review` on a PR from a user with
  write access: force a run.
- `overload review` CLI and a "Re-run" button in the UI.
- Record every webhook in `webhook_deliveries` even if skipped, with the
  skip reason, so behavior is visible in the UI.

### 7.2 Ingest (HTTP handler)

1. Verify `X-Hub-Signature-256` (constant-time compare) before parsing.
2. In one transaction: insert delivery (`ON CONFLICT (delivery_id) DO NOTHING`;
   if nothing was inserted, return 200), decide skip/run, insert `runs` row,
   `riverClient.InsertTx(ctx, tx, ReviewPRArgs{RunID: ...}, opts)`.
3. Unique job options by `(repository_id, pr_number, head_sha)`.
4. Supersede: when a new head SHA arrives for a PR, mark older queued runs
   `superseded` and cancel their River jobs; a running job checks for
   supersession before posting and exits without posting.
5. Respond 202 quickly. No GitHub API calls in the handler.

### 7.3 ReviewPRJob (worker)

1. Load run and repository config; set status `running`.
2. Mint an installation token. Fetch PR metadata, the file list with patches,
   and the diff. Enforce limits (max files, max changed lines, max file size,
   ignore globs such as lockfiles, generated and vendored code). If over the
   limit, finish as `skipped_too_large` and optionally post a short note.
3. Build `ReviewSpec`: parsed diff, per-file hunks, repository instructions
   (`.overload.yaml` or `.github/overload.md` at the base SHA, see 7.6), model
   profile snapshot, mode, budgets.
4. Start sandbox. Stream the head tarball in, upload `spec.json`, extract,
   run `overload-agent` with a timeout, download `result.json`. Always `Close`
   in a `defer` with a fresh context.
5. Validate findings (7.4). Store all findings, including suppressed ones
   with a reason.
6. Unless dry run or superseded, post one GitHub review (`event: COMMENT`)
   with inline comments and a short summary body. Store GitHub comment IDs.
7. Record metrics and set final status. Every step appends a `run_events`
   row so the UI shows a timeline.

Retries: infrastructure errors (sandbox claim, network, 5xx) retry with
River backoff, max 3 attempts. Model or parse failures are recorded on the
run and not retried beyond the harness's own single repair attempt.

### 7.4 Finding validation and rendering

- Schema: `path`, `line`, `side` (`RIGHT` only in v1), optional `start_line`,
  `severity` (`critical|high|medium|low`), `category` (`bug|security|performance|correctness|maintainability|test`),
  `title`, `body`, `confidence` (0 to 1), `evidence` (quoted code).
- Drop findings whose path is not in the diff or whose line is not a
  commentable line in a hunk (`diff` package). GitHub returns 422 otherwise.
- Drop findings whose `evidence` does not appear near the given line
  (catches hallucinated locations; small line drift may be corrected).
- Filter by per-repo minimum severity and confidence; cap comments per
  review (default 10) sorted by severity then confidence.
- Dedupe within the run, and against earlier overload comments on the PR using
  a fingerprint (path + normalized title + category + line bucket) stored in
  a hidden marker `<!-- overload:fp=... -->`.
- Sanitize bodies: length cap, neutralize `@mentions`, strip HTML and images,
  no links other than to the overload run page.

### 7.5 Harness modes

All modes share a pre-built **context bundle** that the harness assembles
itself from the extracted repo: the diff, each changed file's surrounding
code (enclosing function or a line window), and cheaply found related code
(definitions of symbols referenced in changed hunks via `rg`). The bundle is
trimmed to the model's context budget with a deterministic priority order.

1. **context**: one model call with the bundle, structured output. No tools.
   Baseline and fallback.
2. **plan**: call 1 asks for a structured list of up to N extra files or
   symbols to inspect; the harness fetches them; call 2 produces findings.
3. **tools**: Fantasy agent loop with read-only tools (`read_file`,
   `search`, `list_dir`, `get_diff`), a step limit (default 8) and a token
   budget, then a final structured findings call.

**Decision (2026-10-04): no tool calls in the default path.** Tool-calling
coordinators are where agent runs most often go wrong, and local models are
weaker at multi-turn tool use; each extra turn also costs minutes on a Mac.
Every model call in the review pipeline is "text in, JSON out" (see 7.8).
The **tools** mode stays a possible opt-in for tool-capable hosted models
later; it is not on the roadmap.

Structured output: prefer Fantasy's structured-object support when the
provider supports it, and pass a JSON schema (`response_format`) to
`llama-server`/Ollama through provider options. Always validate against the
schema; on failure do one repair call; on second failure record
`model_output_invalid`. Tool calling against llama.cpp needs `llama-server`
started with `--jinja`. Document this and detect missing tool support
(fall back to `plan` mode and record it).

Tools must: resolve paths inside the repo root only (reject `..`, absolute
paths, symlinks leaving the root), cap output size, and cap `rg` runtime.

Prompts live in `harness/prompts/*.md` (embedded) with a version string
recorded on every run so evaluations can compare prompt versions.

### 7.6 Per-repository configuration

Stored in the database (editable in UI) and optionally overridden by a file
in the repository at the **base** SHA (never the head, so a PR cannot change
its own review rules): `enabled`, `dry_run`, `model_profile`, `mode`,
`min_severity`, `min_confidence`, `max_comments`, `ignore_globs`,
`review_drafts`, free-form `instructions`.

### 7.7 Configuration redesign: jobs, prompts and triggers

**Problem in the current implementation (2026-09-29).** The Settings page
combines a model, a fixed built-in prompt selector and four optional review
instructions in one form. The selector is not an editable prompt: `context`
and `switchboard-go` are embedded system prompts. Every configured pass gets
that same system prompt with its instructions appended, makes independent
per-file JSON calls, and uses the same model. The GitHub webhook path checks
only `repositories.enabled` and `dry_run`; it does not select a profile or
resolve prompts. The River review worker completes with a no-op. There are no
schedule definitions, cron enqueues or Sentry/generic webhook handlers. Do not
present any of those paths as operational until they actually execute jobs.

**Terminology and ownership.** Make each concern editable in its own place:

| Concept | Purpose | Owner |
|---|---|---|
| Model connection | Provider adapter, endpoint, model ID, secret reference, capabilities and budgets | Workspace |
| Prompt template | Named/versioned editable text with explicit variables and type (`review`, `plan`, `verify`, see 7.8); built-ins are seeded templates, not hard-coded choices | Workspace |
| Agent definition | Named role, model connection, prompts, limits and output contract; used as a main agent or sub-agent (7.8) | Workspace |
| Workflow | One main agent, sub-agents with scopes, skip globs, file-review limit and failure policy (7.8) | Workspace |
| Binding | Which workflow runs for a source/event/repository, enabled/dry-run policy and filters | Repo or workspace |
| Schedule | Cron expression, timezone, target workflow, bound inputs and enabled state | Workspace |

A **model connection is not an agent**. An agent is a model and one prompt
that describes its role, task and allowed context; different focus areas are
different agents, combined as sub-agents in a workflow (7.8). Keep the output JSON contract and untrusted-input boundary in a separate
system-controlled envelope that editable prompts cannot remove. The *final
prompt* shapes a cross-pass summary but never bypasses finding validation.
For a PR workflow, an initial context/triage step is optional; independent
review passes may use different prompts and model connections, then merge into
a final result. For a scheduled or incident workflow, inputs and outputs have
different schemas, not fake PR numbers or file locations. Preserve the current
single-pass behavior as a migrated starter workflow.

**Configuration resolution.** Match a verified source and event (`github`,
`pull_request`, `opened` etc.) to an enabled binding. A GitHub PR binding can
optionally target an exact `owner/repo`; a repo-specific binding overrides a
workspace default, and an explicit CLI dry-run override takes precedence over
both. If multiple bindings of the same specificity match, reject the ambiguous
configuration rather than silently picking one. No matching binding means a
recorded skip, never an implicit model call. Repository membership is verified
from the trusted GitHub installation/delivery, not from PR text. Freeze the
resolved workflow, agent models, prompt *versions and content hashes*, allowed
input schema, head/base SHAs and dry-run policy into the run before enqueueing.
Edits after enqueue must not change a running job. Repository-provided files,
if supported later, are loaded only at the base SHA and are lower-trust
instructions; they cannot change the selected model, destination or policy.

**Source and job boundary.** An authenticated adapter validates signatures,
dedupes a delivery and extracts a bounded, typed event envelope (source,
event, action, installation/repository identity, occurrence time and safe
references). The payload is not a prompt. A binder resolves a workflow; one
transaction records receipt, outcome, immutable run snapshot and River job.
The generic worker dispatches on job kind, fetches the appropriate bounded
context and executes the pinned workflow in a sandbox. The shared run lifecycle
and UI handle PR reviews, scheduled prompts and future incident jobs without
forcing their results into the PR `findings` schema. Typed outputs are
validated at the boundary; only explicitly configured destinations can post
results. Preserve `dry_run` and no-posting defaults across every source.

**Scheduling.** Persist cron schedule, IANA timezone, workflow binding and
`next_run_at`; use a leader-elected scanner that locks due schedules with
`FOR UPDATE SKIP LOCKED`, enqueues the immutable run in the same transaction,
and advances the next occurrence. Unique `(schedule_id, scheduled_for)` prevents
double execution across restarts or multiple server replicas. Define DST,
missed-run/backfill, pause/resume, concurrency, retry and failure behavior
explicitly in tests. River's `PeriodicJobConstructor` is non-blocking and
cannot query this database or build a run there; use it only as a wake-up tick,
not as the dynamic per-schedule persistence mechanism. A schedule targets a
workflow and a typed input, not a GitHub PR webhook impersonation.

**Safety.** Store only environment/secret-manager *references*, not secret
values in tables, prompt bodies, resolved specs, metrics, logs or downloadable
artifacts. Show prompt previews with variable expansion using redacted sample
inputs. Authenticate every configuration mutation, protect forms against CSRF,
limit prompt and payload size, constrain model endpoint destinations (including
SSRF and redirects for non-loopback deployments), and authorize per-repository
bindings. Webhook adapters have distinct signing secrets and replay windows.
Untrusted webhook bodies and repo content remain data, never instructions.
Avoid persisting full incident payloads or stack traces by default; use bounded,
redacted references and retention controls.

### 7.8 Main agents and sub-agents

**Why.** Today a workflow is a flat list of up to four agents ("passes").
Each one reviews every changed file with its own model and prompt, none
sees another's output, and findings carry no record of which agent produced
them. That is several independent reviewers, not a coordinated review. It is
also slow on local models: three agents over a 21-file PR at 8 to 12 minutes
per file is 8 to 12 hours. Sub-agents must review the files they are for.

**Approach: a configured tree, no tool calls.** Run-time delegation, where
a coordinating agent starts children through tool calls (the style of
platforms such as Overmind), is the most flexible design and the least
reliable, especially on local models. Overload trades some flexibility for
speed and reliability: the structure is configured, routing is
deterministic (path globs) with an optional JSON planner, and every model
call is "text in, JSON out".

```
Workflow "go-service-review"
  Main agent "lead"          model ds4-qwen38   prompt "lead-review"
    planner (optional)       prompt "lead-plan"
    verifier (optional)      prompt "lead-verify"
  Sub-agents
    "security"    model gpt-6-sol    scope **/auth/**, **/*sql*   always
    "migrations"  model ds4-qwen38   scope postgres/migrations/**
    "tests"       model ds4-qwen38   scope **/*_test.go            when planned
  Skip globs: **/*.lock, **/vendor/**, **/*_templ.go
  Limit: 60 file reviews per run
```

**Data model.**
- A workflow has exactly one **main agent** and zero to eight **sub-agents**.
  Depth is two: sub-agents cannot have sub-agents.
- **Main agent**: model connection, prompt, optional planner prompt,
  optional verifier prompt, and whether it reviews files no sub-agent
  claimed (default yes).
- **Sub-agent**: model connection, prompt, scope, and a cap on findings
  (default 5). Scope is a list of path globs plus a mode: `always` (every
  file matching a glob), `globs` (matching files, planner may add more) or
  `planned` (only files the planner assigns).
- **Workflow**: skip globs (lockfiles, generated and vendored code) and a
  limit on total file reviews per run.
- Prompt kinds become `review`, `plan` and `verify`; the existing `entry`
  and `review` (pass focus) prompts migrate into them. The system-controlled
  envelope (untrusted-input boundary, output contract) still wraps every
  editable prompt.
- The run snapshot gains a version field and the tree. `Verify()` checks
  prompt digests for every node. Snapshots already queued in the old flat
  format still load.
- Findings get an `agent` column. Duplicates found by several agents are
  kept once and record every agent that found them.

**Pipeline for one PR.**
1. **Skip**: files matching skip globs are dropped and listed on the run.
2. **Globs**: each file is assigned to every sub-agent whose `always` or
   `globs` scope matches.
3. **Planner** (optional, one call): input is the file list, per-file change
   stats, hunk headers (the functions a diff touches) and the first few
   changed lines of each file, plus each sub-agent's name and description.
   Output is JSON: `{"assign": {"path": ["sub-agent", ...]}}`. Rules:
   - it can only **add** assignments, never remove what globs assigned;
   - unknown files or sub-agents, invalid JSON, or an empty reply mean the
     plan is ignored and the run is marked "routed by globs only";
   - if the plan exceeds the file-review limit, planner assignments are
     dropped first, never glob assignments;
   - the validated plan is saved on the run and shown in the UI.
4. **Unclaimed files** go to the main agent (unless disabled). Only path
   globs claim a file; planner assignments never take a file from the main
   agent. No file is left unreviewed except through skip globs.
5. **Reviews**: each (agent, file) pair is today's per-file review call.
   Sub-agents see only their assigned files.
6. **Verifier** (optional): one call per sub-agent finding with that file's
   diff and content; the reply is `keep` or `drop` with a reason. It may not
   rewrite findings, so cross-run fingerprints and duplicate suppression
   still work. Dropped findings are kept on the run, marked dropped.
7. **Overload** validates every finding against the diff, removes
   duplicates, records attribution, and is the only component that posts to
   GitHub.

**Failure handling.** A file an agent cannot review (a timeout, an
unreadable reply) does not fail the review silently or pass silently: the
run completes as **degraded**, names the agent and the files it did not
review, posting includes a note that the review is partial, and the commit
status says "Partial review". This applies to the main agent too
(2026-10-15: one 38 KB file that Grok 4.7 could not finish in 15 minutes
failed every review of a PR). An agent stops after 3 failed files (a bad
key, a model that is down), and a main agent that reviewed none of its
files still fails the review. A failed planner falls back to globs. A failed verifier
keeps all findings and marks the run degraded.

**Model connections, memory and parallelism.**
- A sub-agent points at a model connection, which is a URL. Five sub-agents
  on one `ds4-server` share one copy of the weights; each concurrent request
  adds only its context (about 1 GiB for Qwen3.8 at 32k context).
- The parallel limit belongs to the **model connection**, not the agent: a
  shared limiter per connection caps in-flight requests across every agent
  that uses it, and queues the rest. Agents on different connections (one
  local, one hosted) run at the same time. This replaces today's
  per-agent-in-sequence execution.
- Memory multiplies only with **different** local models, since each needs
  its own server. One local model plus hosted sub-agents is the recommended
  mix. The preview warns when the local models a workflow uses are unlikely
  to fit in memory together.
- For a connection with Parallel files above 1, the setup command suggests
  `ds4-server --batched-session N` (or llama-server `-np N`).

**Preview.** The workflow page shows, for a sample PR or a pasted file list,
which agent reviews which files, the number of model calls, the expected
parallelism per connection and a rough duration. The run page shows the
same breakdown as executed: files per agent (by glob or planner), skipped
files, findings and tokens per agent, and the verifier's decisions.

**Migration.** Existing workflows become a main agent (the first agent, no
planner) with the remaining agents as `always` sub-agents with no globs,
which keeps today's behaviour. Legacy per-model passes become sub-agents of
that model's starter workflow. The word "pass" is removed from the UI and
CLI. The CLI and JSON configuration expose the same tree.

**Delivery stages.**
1. Done: the first agent of a workflow is its main agent and the rest are
   sub-agents (up to 8); all agents' file reviews are scheduled together
   with one parallel limit per model server (base URL and model), so agents
   on different servers run at the same time; findings record every agent
   that reported them (`findings.agents`), including duplicates collapsed
   across agents; per-model passes became sub-agents of their starter
   workflow (migration 017) and `--agents-json` is gone. The snapshot
   version field moves to stage 2, where the snapshot format first changes.
   UI wording ("pass") is left to stage 4 because the UI is being reworked
   separately.
2. Done: workflows have skip paths, per-sub-agent scopes (path globs and a
   findings cap), a main-agent mode (`all` or `unclaimed` files) and a
   file-review limit, stored in `workflows.routing` (migration 018) and
   pinned in snapshot version 2; version 0 snapshots keep every agent on
   every file. Skipped files are dropped before the context-size check, so
   a large lockfile no longer fails a review. A run that would exceed the
   limit fails before any model call. Scope modes are reduced to "globs" for
   now (no paths means every file); `always`/`planned` arrive with the
   planner in stage 3. The findings cap is opt-in (0 means no cap) so
   migrated workflows keep their behaviour. The run page shows files,
   findings, findings over the cap and tokens per agent plus skipped files;
   the preview (workflow page link, and `overload workflows preview NAME`
   reading a file list) shows routing, model calls, parallelism per model
   server, a rough duration and a warning when several local models must be
   loaded at once. Previewing a sample PR by number is left to stage 4.
   Scopes and limits are edited through `overload workflows apply` until the
   stage 4 UI; saving a workflow from the current form keeps them.
3. Done: a workflow can name a planner prompt and a verifier prompt (new
   prompt kinds `plan` and `verify`, migration 019), pinned to the revision
   current when the workflow is saved and run on the main agent's model;
   snapshot version 3. Scopes gain `mode` (`globs` default, `always`,
   `planned`) and a `description` the planner sees. The planner gets the
   plannable sub-agents and, per reviewable file, change counts, hunk headers
   and the first five changed lines (24 KB budget; files past it are listed
   by name only) and replies `{"assign": {...}}`. Invalid JSON, a missing
   `assign`, unknown or skipped files, or the main agent, unknown or
   `always` sub-agents reject the whole plan; a failed call or a rejected
   plan routes by globs only and says why on the run. Over the file-review
   limit, planner assignments are dropped from the end. The verifier checks
   findings no main agent reported, one call each with that file's bundle;
   dropped findings are stored with status `dropped` and the reason, shown
   on the run and never posted. A failed sub-agent (not the main agent) or
   verifier call no longer fails the review: it completes degraded, the run
   page names the agent and its unreviewed files, and the posted review
   says it is partial without internal details. Run status stays
   `completed`; `metrics.degraded` and `routing.degraded` mark it. The
   planted-bug eval gained `TestPlannerRoutingEval`: SQL code in
   `util/textutil.go` must reach a database sub-agent scoped to
   `**/store/**` through the planner.
4. Done: the workflow page edits the tree: a main agent (with its review
   mode and the planner and verifier prompts), up to eight sub-agent slots
   (agent, file mode, paths, planner description, findings cap; scope chips
   summarize each, unused slots stay hidden behind "Add sub-agent" with
   JavaScript and the up/down buttons move a whole slot), and the skip
   paths and file-review limit. Validation errors are shown in the form.
   The workflow list shows the main agent and sub-agents with their paths.
   The preview takes a configured repository and pull request number (the
   server fetches the changed files with the repository's GitHub App
   installation or the token) or a pasted file list. "Pass" is gone from
   the UI.
5. Done: agents have one prompt. The optional review focus prompt was
   removed (migration 020): each agent that had one got a new prompt with
   both texts joined as reviews joined them, so behaviour is unchanged, and
   former focus prompts became ordinary agent prompts. Focus areas are now
   separate sub-agents. Snapshots pinned earlier still carry and verify
   their focus prompt.
6. Done: agents and workflows own their instructions (migration 021). The
   agent page has an Instructions box (new agents can start from a built-in
   prompt) and the workflow page has Planner and Verifier boxes; the
   Prompts page and `overload prompts` are gone. Changed text is saved as a
   new version that the agent or workflow uses from then on, and runs keep
   the text they started with. The migration gave each agent and workflow
   its own copy of the text it used, so agents that shared a prompt now
   have identical copies; old shared prompts stay in the database, unused.
   Agent JSON takes `prompt` text; `planner_prompt` and `verifier_prompt`
   are text. The UI refuses to create an agent or workflow whose name is
   already taken, since that would overwrite its instructions.

Each stage keeps `make ci`, the macOS install test and the kind sandbox
test passing, and extends the planted-bug eval: a multi-file fixture where
a file with a misleading name must still reach the right sub-agent through
the planner.

**Later, opt-in.** Run-time delegation as a main-agent mode for
tool-capable hosted models, using the same allowed sub-agents and limits.
Not part of v1.

### 7.9 Tools for scheduled agents

**Decision (2026-10-08): scheduled agents call MCP servers; PR reviews
never do.** A tool server is a name, a streamable-HTTP MCP URL, the name of
an environment variable holding its bearer token (prefix `OVERLOAD_TOOL_`,
so it cannot name overload's own secrets) and a description. Agents of the
scheduled-prompt kind list the tool servers they use; validation refuses
tools on PR review agents and snapshots, because reviews read untrusted
pull request text.

- The run snapshot pins each agent's tool servers (never the token). A
  disabled or deleted server makes the workflow unavailable instead of
  running the agent without it.
- At run time overload opens one MCP session per server
  (`modelcontextprotocol/go-sdk`, stateless-friendly, no standalone SSE),
  lists its tools and offers them to the model as `<server>_<tool>`. Each
  run sends `X-Switchboard-Session-Id: overload-run-<id>-<agent>` so
  Switchboard's pins and context stay within the run.
- The agent loop is Fantasy's: a step is one model call. Limits come from
  the workflow: `max_steps` (default 40, max 200) and `timeout_minutes`
  (default 30, max 240), plus at most five tool calls per step. On the
  last step tools are removed and the agent is told to report. Tool
  results are cut to 32 KB; tool failures go back to the model as errors.
- Every tool call is a run event with its input and result (cut to 4 KB
  and 8 KB); the agent's output records steps, tool calls and tokens.
- Schedules can be run by hand (`overload schedules run NAME`, **Run
  now**), which queues a run with trigger `manual`.
- Model connections gained `api: responses`: newer OpenAI reasoning models
  (GPT-6.1 Sol) reject tools with `reasoning_effort` on Chat Completions.
  Fantasy only sends reasoning settings for model names it recognizes
  (`gpt-5`, and since 0.45 `gpt-6` and later), so overload sets
  `reasoning.effort` on each request, which works for any model behind the
  Responses API.

Verified against a self-hosted Switchboard (GitHub) and hosted Switchboard
with GPT-6.1 Sol through Cloudflare AI Gateway. The Switchboard
integration research job runs as an overload schedule
(`switchboard-integration-research`, prompt in `docs/examples/switchboard-integration-research.md`).

### 7.10 Workspace backends and the model proxy

**Decision (2026-10-09): overload does not run workspaces itself.** Work
that needs a checkout, a shell, Docker or a browser of the branch under
test runs on a backend that already isolates and cleans up (GitHub
Actions, Agent Sandbox, a hosted sandbox service, a trusted dev box).
Overload schedules it, starts it, and records the outcome. A backend that
exposes MCP tools can later be wired as a per-run tool server, so agents
step through it under overload's limits.

- First backend, by hand: GitHub Actions. Switchboard's
  `overload-implement.yml` (`workflow_dispatch`, one pass per run, never
  merges) replaces the Orca implementation job; an overload schedule
  (`switchboard-implement`, prompt in
  `docs/examples/switchboard-implement-dispatch.md`) decides whether to
  start it and reports the previous run.
- Agent Sandbox now works with hosted models: the **model proxy**
  (`modelproxy`, port 8083, `OVERLOAD_MODEL_PROXY_URL`) gives each sandbox
  a short-lived per-model URL; it adds the API key and forwards only
  `chat/completions` and `responses` for the granted model. Sandbox pods
  may reach overload only on 8083. The agent image is published to
  `ghcr.io/daltoniam/overload-agent` with each release.

## 8. Data model (initial)

```
github_installations(id, account_login, account_type, created_at, updated_at, suspended_at)
repositories(id, installation_id, github_id, full_name, enabled, dry_run,
             model_profile_id, config jsonb, created_at, updated_at)
model_profiles(id, name unique, provider, base_url, model, api_key_env,
               mode, params jsonb, is_default, created_at, updated_at)
webhook_deliveries(id, delivery_id unique, event, action, repository_full_name,
                   payload jsonb, outcome, skip_reason, run_id, received_at)
runs(id, kind, repository_id, pr_number, head_sha, base_sha, trigger,
     status, mode, model_profile jsonb, prompt_version, sandbox_name,
     river_job_id, dry_run, error_code, error_message, metrics jsonb,
     eval_case_id, created_at, started_at, finished_at)
run_events(id, run_id, at, level, step, message, data jsonb)
run_artifacts(run_id, name, content bytea/jsonb)   -- spec, result, transcript
findings(id, run_id, path, line, start_line, side, severity, category,
         title, body, confidence, evidence, fingerprint, status,
         suppressed_reason, github_comment_id, human_label, labeled_by, labeled_at)
eval_sets(id, name, description, created_at)
eval_cases(id, eval_set_id, repository_full_name, pr_number, head_sha,
           base_sha, expected jsonb, notes)
eval_runs(id, eval_set_id, model_profile_id, mode, prompt_version,
          status, summary jsonb, created_at, finished_at)
```

`runs.kind` is `pr_review` in v1. The shape is generic so later job types
(generic webhook, cron prompt) reuse runs, events, artifacts and the UI.
API keys are never stored in the database; profiles reference an environment
variable or Kubernetes Secret key by name.

### 8.1 Planned additive configuration schema

Keep the existing `model_profiles` table and its data. Migrate its
`prompt_profile` and `agents` JSONB into a starter workflow without deleting
profiles or prior run artifacts. New tables and columns should be introduced
through versioned, transactional migrations:

```
prompt_templates(id, name, kind, current_revision_id, created_at)
prompt_revisions(id, template_id, revision, body, content_sha256, created_at)
agent_definitions(id, name, model_profile_id, entry_prompt_revision_id,
                  output_kind, limits_json, enabled, created_at, updated_at)
workflows(id, name, job_kind, revision, steps_json, failure_policy,
          output_kind, created_at, updated_at)
trigger_bindings(id, source, event, action, repository_id nullable,
                 workflow_id, enabled, dry_run, filters_json, created_at)
schedules(id, name, workflow_id, cron, timezone, input_json, next_run_at,
          enabled, overlap_policy, created_at, updated_at)
schedule_occurrences(schedule_id, scheduled_for, run_id,
                     unique(schedule_id, scheduled_for))
```

Use typed validated step/binding/input structs, not arbitrary executable JSON.
Extend `runs` additively with nullable `workflow_id`, `binding_id`,
`schedule_id`, `source_event_id`, `config_snapshot` and `scheduled_for`;
retain existing `repository_id`/`pr_number` data for PR history, but allow
NULL for non-PR jobs in a migration. Separate `job_outputs` keyed by run ID
and schema kind from PR-only `findings`. Extend `webhook_deliveries` with
`source` and scoped uniqueness on `(source, delivery_id)` so provider IDs do
not collide; migrate existing rows to `github`. Enforce FK and uniqueness
rules without dropping existing tables. Keep immutable prompt revisions so
editing a template cannot alter a queued run or historical evaluation. Secret
references in model profiles are operator-controlled and redacted in APIs.

## 9. Evaluation

This is an early milestone, not an afterthought. Quality per dollar decides
which model and mode to use.

- An eval set is a list of PRs (repo, number, **frozen base and head SHAs**)
  with **expected findings** (original diff path, line range, description,
  severity, review comment and fix provenance). Seed it from about 50 real
  PRs, including ones where a human reviewer caught and the author fixed a
  real bug. Never evaluate against the latest PR head when measuring an
  earlier review; it leaks fixes into the model input. Freeze model/prompt
  settings and run the model before reading reviewer labels, then adjudicate
  whether each comment was an actual bug rather than treating all comments
  as ground truth.
- `overload eval run` creates one run per case per (profile, mode) through the
  same queue and sandbox path, always in dry run.
- Scoring: a finding matches an expected finding when paths match and lines
  overlap within a tolerance; the UI lets a human confirm or reject unmatched
  findings (`human_label`: `valid`, `invalid`, `nit`). Labels on real runs
  feed the same metrics.
- Report per (profile, mode, prompt version): recall of expected findings,
  precision (valid / labeled), comments per PR, invalid-location rate,
  schema-failure rate, p50/p95 latency, tokens, estimated cost.
- Initial matrix: Qwen3-4B-Instruct-2507, Qwen3-8B, Ternary Bonsai 8B, and
  Qwen3-Coder-30B-A3B-Instruct when a GPU or large-memory host is available;
  Recursive Flint (40B-A2.5B, llama.cpp GGUF, Q4_K_M and Q8_0 at its native
  256k context) is measured on the planted-bug eval (3 to 4 of 5, see
  deploy/README.md), as are Liquid's LFM2.5-24B-A2B (1 to 3 of 5) and
  LFM3-1.7B (1 to 2 of 5, invalid JSON in most runs), and the non-Chinese
  models in 16.5 (Gemma 4, Granite 4.2, Nemotron 3.5, North Mini Code,
  Devstral Small 2); modes context, plan, tools.

## 10. UI

Server-rendered with templ, following Switchboard's structure and styling
conventions (`layouts.Base(PageData)`, shared components, one file per
page). Plain forms and links first; add minimal progressive enhancement only
where it clearly helps (for example auto-refreshing a running run's timeline).

Pages:

- **Dashboard**: queue depth, running runs, recent runs, failure count, sandbox health.
- **Runs**: filterable list; detail page with status timeline, PR link,
  model profile, mode, metrics, findings (posted and suppressed with reasons),
  transcript, spec/result downloads, Re-run and Cancel actions, finding labels.
- **Repositories**: installed repos, enable toggle, dry-run toggle, config form.
- **Model profiles**: CRUD, set default, "Test connection" (small completion
  and a tool-call probe that reports tool-call support).
- **Evals**: sets and cases, launch a matrix, comparison table.
- **Webhooks**: recent deliveries with outcome and skip reason.
- **Settings**: GitHub App status (app ID, installations, webhook secret set), sandbox config.

### 10.1 Configuration UX (planned)

The present `/settings` form is a temporary local-review convenience, not
the desired mental model. Replace it with a small navigation structure:

1. **Models**: endpoint, model ID, capabilities, secret *reference*, health
   probe. Never place prompts inside a model connection form.
2. **Agents / Workflows**: an agent is a model and its instructions, edited
   on the agent page with a version number and a warning that instructions
   are not a secret store (version history and diffs are a follow-up); a
   workflow's planner and verifier instructions live on the workflow. Show
   the main agent and sub-agents with their scopes in a readable list.
   Provide a starter single-agent PR workflow; advanced options can be
   progressive.
3. **Repositories**: list installed GitHub repos; each detail page shows
   enabled/dry-run state, matching event actions, bound workflow and effective
   config (workspace defaults vs repo override). Provide a test-match action
   using a sample verified event, without executing a job or posting.
4. **Schedules / Sources**: create a cron schedule with timezone and next
   three fire times, pause/resume and a dry-run `Run now`. List webhook sources
   with auth status and matching bindings. Do not imply Sentry works before an
   adapter exists.
5. **Runs**: group by source/kind/repo/schedule and show the resolved workflow
   and prompt revisions, per-agent step outcomes, deduped output and clear
   skip/failure reasons. From a failed binding, link to its configuration.

All these resources must be manageable through a local CLI (`models`,
`agents`, `workflows`, `bindings`, `schedules` with list/show/apply
or set/disable), using the same validation and persistence as the UI. Favor
versioned declarative JSON/YAML input for agents and repeatable installs; do
not expose an unauthenticated settings API. A guided setup path should be:
connect local model, create/edit a prompt, configure an agent, bind it to a
repo PR event, dry-run a sample, and only then enable automatic reviews.

Security: basic auth on all UI routes (webhooks excluded), CSRF tokens on
state-changing forms, `SameSite=Strict` cookies, strict CSP.

## 11. Deployment

- **Mac (primary)**: `install.sh` or `brew install daltoniam/tap/overload`,
  then `overload install`. Details in `deploy/README.md`.
- **Linux server**: `overload install --database-url ...` writes the config;
  run `overload serve` under systemd or the GHCR image.
- **Kubernetes**: Kustomize base, a kind overlay with Postgres, and a sandbox
  component that runs webhook reviews in Agent Sandbox pods. Verified on
  disposable kind clusters by `make kind-test` and `make kind-sandbox-test`.
- **Kubernetes behind Cloudflare** (`overlays/cloudflare`, 2026-10-08): no
  Ingress or public IP. cloudflared pods carry the tunnel, a NetworkPolicy
  lets only them reach the app, Cloudflare Access guards the UI and overload
  verifies the Access token itself, and only `/webhooks/github` bypasses
  Access. Running on DigitalOcean (`chapterchamp-k8s`, namespace
  `overload`, `overload-review.daltoniam.com`) with Cloudflare AI Gateway
  models, reviewing `daltoniam/switchboard` and `Vluxe/Switchboard-hosted`.
  Guide: `docs/cloudflare-kubernetes.md`.

## 12. Status and roadmap

Done and tested:
- Mac installer with launchd services and a private Postgres; CI installs and
  reinstalls it on macOS. Homebrew formula and release archives via
  GoReleaser.
- One review engine: every review runs a pinned workflow (saved models with
  built-in prompts become one-agent workflows). Per-model reasoning style,
  effort, output budget and parallel files; local connections have no
  response-header timeout.
- DwarfStar ds4 and llama.cpp measured on a planted-bug file (see
  `deploy/README.md`): Qwen3.8 Flash Next on ds4 found 5/5 in every run.
- GitHub App setup from a manifest with one-time state, installation and
  repository sync keyed by GitHub repository ID, installation pinned per run,
  signed webhooks, opt-in posting with run markers and line-independent
  fingerprints, model text stripped of links and mentions.
- Scheduled prompts with cron and timezones through the same model settings.
- Kubernetes with Agent Sandbox: sandboxed reviews, network isolation and
  claim cleanup verified on kind.
- UI hardening: Host allowlist, same-origin checks on every state change,
  constant-time form tokens, server timeouts.

- Main agents and sub-agents (7.8), all four stages. Smoke-tested end to
  end on a fresh database: model, prompts, agents and a three-agent
  workflow configured in the UI (checked with Playwright), then a dry-run
  review of a real open PR (switchboard #177, 5 files) on bonsai-2-27b via
  llama.cpp: README skipped, test file to the `always` sub-agent, planner
  assignments applied, 17m43s, run page and result download correct. The
  live planner eval with a verifier routed the misleading file, found the
  SQL injection and the verifier kept all 5 real findings (21m22s).

- Pre-release review (YAGNI, parse-don't-validate, data-model coherence,
  SOLID/DRY, security and tenancy, chaos edge cases, functional core):
  fixed every bug and security finding that affects daily use.
  - Diffs: git's quoted non-ASCII paths, the tab after paths with spaces,
    CRLF, and binary, empty, renamed or mode-only files no longer fail the
    review; an added line starting with `++` is no longer read as a file
    header. A file whose diff exceeds the context budget is listed as too
    large and the review completes as partial instead of failing.
  - Findings: each file's reply is validated against that file's diff only
    (findings for other files used to push out real ones under the cap of
    10); the review-wide limit of 10 now runs in the harness, so per-agent
    counts match what is posted, and the overflow is stored as dropped.
  - Security: posting trusts only reviews written by overload's own GitHub
    account (a PR author could forge the run marker and stop posting); a
    signed webhook body is ingested once (a replay under a new delivery ID
    could cancel the latest head's review); sandbox results have their
    dropped findings and routing re-validated (a dropped finding with no
    reason could be posted unsanitized); the verifier never drops critical
    or security findings (PR text reaches its prompt); the PR preview only
    reads enabled repositories and refuses cross-site requests; repository
    names are validated; model API key variables may not name cloud,
    database, password or private-key variables; the sanitizer removes
    reference-style links, `//host` URLs and HTML-entity mentions; a run is
    not posted if its repository was disabled or set to dry run meanwhile.
  - Configuration: an agent's job type cannot change while a workflow of
    the old type uses it; a workflow with a disabled agent records a skip
    reason instead of failing the webhook; workflows are normalized on
    save from the UI and the CLI alike; the CLI rejects input over 1 MiB or
    with more than one object instead of truncating it.

Before the first release:
1. Measure `ds4-server --batched-session` with the per-connection limiter on
   the target Mac Studio (not the M1 Max used so far), with Qwen3.8 and
   DeepSeek V4 Flash, to set defaults for Parallel files.
2. Publish the first release and Homebrew formula (needs the
   `RELEASE_TOKEN` secret for the tap); install on a clean Mac.
3. Run the GitHub App and posting against a real repository through a tunnel.
4. Done: the findings posted per review are a workflow setting
   (`max_findings`, default 10, up to 50; the rest stay on the run as
   dropped), since one number cannot suit both a single local model and
   eight focused sub-agents. The planner's instructions now ask it to be
   conservative, but on switchboard #177 bonsai-2-27b still gave all four
   files (including `main.go`, config and the test file) to one sub-agent,
   so the rule is enforced in code instead: only path globs take a file
   away from the main agent. Planner assignments add reviews and never
   remove the main agent's, so an over-eager planner costs time, not
   coverage. A later #177 run with both changes sent only
   `integrations/intercom/intercom.go` to the integrations sub-agent while
   the main agent reviewed `main.go`, the config and the client (21m32s;
   planner output varies from run to run).
5. Done: a review GitHub rejects (422, for example after a force push) ends
   with post status `post_rejected` and GitHub's reason on the timeline
   instead of being retried for hours. A finished review of an older commit
   is not posted once a newer commit of the PR has its own review
   (`superseded_by_newer_commit`).
6. Done: webhook deliveries for one pull request are ingested one at a
   time (a transaction lock), so simultaneous deliveries for a commit queue
   one review, not several. Saving a workflow or an agent's instructions
   from an older copy is refused with a "someone else changed it" message
   instead of overwriting the newer save (configuration files that omit
   the revision are not checked), and saving an unchanged workflow no
   longer bumps its revision. Each model call has a time limit (local 90
   minutes, hosted 15): a hung call leaves its file unreviewed and the
   review partial (see Failure handling in 7.8).

After:
4. Evaluation: real PRs with known bugs, scored per model and per workflow
   shape, beyond the planted-bug fixtures.
5. A reaper for sandbox claims left by a crashed worker; backup, restore and
   upgrade tests.
6. Sentry and other signed webhook sources for generic jobs.
7. Remote management: today the CLI configures overload by connecting to
   Postgres (`DATABASE_URL`), so an agent must run next to the database
   (`kubectl exec`). Add a token-authenticated JSON API for the same
   resources (agents, workflows, bindings, repositories, schedules, model
   settings, runs) and `overload --server URL` in the CLI, so coding agents
   and automations can manage a remote overload. Behind Cloudflare Access,
   clients authenticate with an Access service token; overload accepts the
   service token's Access JWT like a user's.
8. Tool-using agent jobs (step 1 done 2026-10-08, see 7.9): scheduled
   agents call MCP tools (Switchboard), which covers the Orca integration
   research job (issues only). The Orca implementation job still needs a
   workspace: a repository checkout, a shell to run `make ci` (which
   starts Docker Compose), git push with scoped credentials, and a browser
   for UI tests. Next: a job kind that runs a coding agent (Crush) in a
   sandbox with those, with the transcript and outputs on the run.

## 13. Follow-ups (post v1, keep interfaces ready)

- **Code structure** (from the pre-release review; no behaviour change):
  typed review metrics instead of `map[string]any`; one store method for
  finishing a run shared by the worker and local reviews; GitHub source and
  posting settings built once in `cmd/overload` instead of read from the
  environment inside workers; explicit `httpapi` dependencies instead of
  optional-interface assertions; per-resource POST handlers; a compiled
  routing value built once per review; separate drop and suppression
  reasons for findings; typed enums for kinds and modes.
- **Model connections**: bind a key variable to its host so editing a
  connection's URL cannot send an existing key elsewhere; refuse metadata
  and link-local addresses for hosted connections.

- **Cloud model providers**: Anthropic's Messages API is supported
  (`--provider anthropic`); Fantasy also supports Bedrock, Google and
  OpenRouter natively; add them to model profiles as needed. Prefer an
  overload-hosted model gateway (a proxy in the control plane for
  OpenAI-compatible and Anthropic Messages requests)
  so API keys never enter sandboxes and token usage is metered centrally.
  Native Anthropic support matters for quality, not just choice: through an
  OpenAI-compatible endpoint (including Cloudflare AI Gateway's) Claude
  models run without thinking, and Sonnet 5.5 found 9 of 20 planted bugs
  that way against 14 with `output_config.effort: high` and adaptive
  thinking (deploy/README.md).
- **CLI harnesses** (Claude Code, Crush, Codex): alternate agent images that
  run the CLI and emit `result.json` in the same schema.
- **Generic jobs**: signed source adapters (starting with Sentry), typed
  incident/other job outputs and DB-backed cron schedules. Use a leader-elected
  due-schedule scanner with transactional enqueue, not a blocking River
  periodic constructor (see 7.7).
- **Switchboard / MCP** tools for agents.
- Kueue for cluster-level quota, KEDA scale-to-zero for warm pools, River UI,
  Helm chart, tiered review (small model triage, larger model on flagged PRs).

## 14. Engineering conventions

- `gofmt`, `go vet`, `golangci-lint` clean; CI rejects unformatted code and
  stale `*_templ.go` files (generated files are committed, as in Switchboard).
- Table-driven unit tests; interfaces faked in tests; Postgres integration
  tests use `DATABASE_URL` and a per-test schema or transaction; kind tests
  behind the `e2e` build tag; model calls in tests use recorded responses
  (`testdata/`) or a fake OpenAI-compatible server, never a live model.
- `log/slog` structured logging with `run_id` on every line; never log
  tokens, keys or full webhook payload secrets.
- Contexts and timeouts on every external call; sandbox cleanup uses a
  fresh context so it runs after cancellation.
- No em dashes in source code; comments only where they explain why.
- Pinned versions: Agent Sandbox v1.0.4 (SDK supports `sandboxd` for custom
  images), River v0.47.0, Fantasy v0.40.0 (v0.41.3 needs Go 1.26.6 and
  v0.42+ needs Go 1.27), templ v0.3.1001, golangci-lint v2.10.1.
  Update Fantasy when the chosen Go toolchain can build a newer release.

## 15. Open questions

- Whether `ds4-server --batched-session` makes parallel reviews faster on the
  target Mac Studio, and which DwarfStar model gives the best review quality
  per minute there. On the M1 Max, two parallel llama.cpp requests were
  slower in total than one (10 vs 13 tokens/s); ds4 batching is unmeasured.
- How often the planner adds a useful assignment that globs missed, and
  whether its extra context (hunk headers, first changed lines) is enough;
  measure before making it a default. On switchboard #177 bonsai-2-27b
  assigned every non-skipped file (including config and wiring) to the
  integrations sub-agent even when told to be conservative. Since planner
  assignments became additive this costs extra reviews rather than
  coverage; a stronger local model, or a per-sub-agent cap, may cut the
  cost.
- Whether the verifier removes more false positives than true findings on
  local models.
- Whether a hosted model proxy (keys held outside sandboxes, per-run tokens)
  should be added so hosted models can run in sandbox mode.

## 16. Product direction: a model host for Apple hardware (exploratory)

Not a v1 commitment; recorded so roadmap decisions keep the door open.

### 16.1 Thesis

Turn overload from a PR reviewer into the software and user layer for teams
that run open-weight models on their own Macs: an IT team racks a few Mac
Studios (or developers keep a Mac mini under the desk), and developers point
their agents (Claude Code, OpenCode, Cline, Crush) and overload's own jobs
at it. Switchboard is the integration layer on top. Buyers are teams that
put security or cost ahead of frontier quality: code may not leave the
building, usage-based AI bills are unwelcome, and fixed hardware that can be
depreciated is easier to approve. Frontier-first companies are not the
target. Local models trail hosted frontier models (5/5 for GPT-6 Sol versus
3 to 5 of 5 locally on the planted-bug eval); the bet is that "good enough,
private and fixed-cost" keeps improving as open models do.

### 16.2 Hardware tiers

Be explicit about hardware; it decides what a customer can run. Speeds are
from our M1 Max (64 GB, 400 GB/s) unless noted; newer chips are faster.

| Tier | Example hardware | What fits | Expectation |
|---|---|---|---|
| Developer machine | MacBook Pro or Mac mini, 32 to 64 GB | 20 to 45 GB mixture-of-experts models: Flint Q4/Q8, Nemotron 3.5 Lightning, North Mini Code; dense ~30B models only at Q4 | One user; reviews in 4 to 25 min per file; dense 30B models at Q8 swapped to about 3 tokens/s on the 64 GB M1 Max with normal dev apps open; long prompts slow (a 205k-token prompt took 55 min to process) |
| Team node | One Mac Studio M5 Max (up to 128 GB) or M5 Ultra (up to 512 GB, 1.2 TB/s) | Everything above at Q8 with parallel slots; Qwen3.8-class models; 100B to 300B models at 4 bits | A small team sharing one queue |
| Team cluster | 2 to 4 Mac Studios over Thunderbolt 5 | 400B to 1T models (Beam 1.0 is about 1 TB at BF16, about 300 GB at 4 bits) | Frontier-adjacent open models for a team |

A 64 GB Mac mini M5 Pro (307 GB/s, Thunderbolt 5) is the smallest node that
can join a cluster; the base Mac mini has Thunderbolt 4 and cannot.

### 16.3 Clustering (state as of November 2026)

- **Transport**: macOS 26.2 added RDMA over Thunderbolt 5 on Apple silicon
  Macs with Thunderbolt 5 (Apple TN3205). It is enabled once per machine from
  Recovery (`rdma_ctl enable`). Links are point-to-point with no switch: a
  four-node full mesh needs six cables; larger rings forward through
  neighbours.
- **Runtimes**:
  - *MLX + JACCL* (MLX 0.30.1+): `mlx_lm.server` under `mlx.launch` serves
    one model sharded across nodes, tensor parallel (faster decode) or
    pipeline parallel (bigger models, chunked prefill). The clearest
    supported path; not every architecture is supported.
  - *exo* (Apache 2.0): discovery, topology-aware placement, tensor and
    pipeline sharding over RDMA, a dashboard, and OpenAI, Responses, Claude
    Messages and Ollama APIs. Friendlier, but every node must run the same
    version and recent releases still fix stuck loads and RDMA regressions.
  - *llama.cpp RPC*: now has an RDMA transport, but upstream calls it a
    fragile, insecure proof of concept; not for production.
- **Published scaling**: Apple reports four M5 Ultras in tensor parallel at
  about 2.6x one node's decode speed for Qwen 3.8 27B and 3.0x for Mistral
  Medium 3.5 128B (65 to 76 percent of linear), and an 816B model at 37
  tokens/s on four nodes. Earlier reports put four M3 Ultras at about 30
  tokens/s on Kimi K2 and Qwen3 235B with exo. Prefill remains the weak
  point; pipeline chunking improves time to first token by up to about 1.8x.
- **Operations**: machines must stay awake (Thunderbolt has no Wake-on-LAN),
  restart after power loss and log in automatically; macOS upgrades can need
  the GUI; MDM is the realistic way to manage a rack. Same OS and runtime
  versions on every node.

Decision: build on the MLX/JACCL server for clusters and llama.cpp for
single machines behind one OpenAI-compatible endpoint; treat exo as a
competitor and possible backend, and revisit after a spike on two real
Thunderbolt 5 nodes.

### 16.4 What the product needs that overload lacks

1. **Runtime manager**: a model catalog with provenance (HQ country, base
   model, synthetic-data sources), license (revenue caps, non-commercial
   terms) and known-good settings (context, sampling, reasoning switch);
   checksum-verified, resumable downloads; pinned runtime versions with
   upgrades (Homebrew's llama.cpp lagged new architectures by weeks); disk
   accounting and cleanup; start, stop and health of model servers.
   Everything this plan's measurements had to do by hand.
2. **Shared serving**: per-developer keys, a fair queue in front of the
   one to four parallel slots a Mac offers, usage in tokens and minutes
   rather than dollars, and OpenAI and Anthropic compatible endpoints so
   agents work unchanged. The model proxy (7.10) is the starting point.
3. **Model bake-offs**: the planted-bug eval as a feature, run against the
   customer's own code, so buyers choose models on evidence (see the
   measurements in deploy/README.md).
   Finding placement matters as much as detection: Gemma 4 26B-A4B and
   Devstral Small 2 found real bugs but cited wrong line numbers, so every
   finding was dropped. Numbering diff lines in the prompt, or re-anchoring
   a finding to the line its evidence uniquely matches, would let more
   models qualify; both change review behaviour and need measuring first.
4. **Cluster control**: node inventory, topology and link health, version
   drift, placement of models across nodes.
5. **Packaging**: a signed installer and launchd services per Mac (today's
   installer is the base) plus a container for the control plane where IT
   prefers it.
6. **Identity (OIDC)**, built on open-source libraries
   (`github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`) against any
   standards-compliant provider (Okta, Entra ID, Google Workspace, Keycloak,
   Authentik, Dex), with no dependency on a hosted auth service such as Clerk
   or Auth0. See 16.7.
7. **The review bot as a merge gate**: a documented recipe for making the
   `overload` commit status a required check, with the safeguards in 16.7.

### 16.5 Model policy

Some buyers require US models; many accept allied (France, Canada) but not
Chinese ones. The catalog labels every model and lets an administrator
restrict by origin. Current open models that fit a 64 GB Mac and are not
Chinese: Recursive Flint (US, trained from scratch, Apache 2.0), Google
Gemma 4, IBM Granite 4.2, NVIDIA Nemotron 3.5 Lightning (US, but trained in
part on data generated by DeepSeek, Qwen and GLM), Mistral Devstral Small 2
(France) and Cohere North Mini Code (Canada). The strongest US open models
(Reflection Beam 1.0, Arcee Trinity-Large-Thinking, gpt-oss-120b) need a
Mac Studio or a cluster. Measured on the planted-bug eval (deploy/README.md):
Flint Q8 found 15 of 20, Gemma 4 31B and North Mini Code 12, Nemotron 8,
Granite 4.2 30B 2 in its one finished run, and Gemma 4 26B-A4B and Devstral
Small 2 almost none because of wrong line numbers; Qwen3.8 found 20 of 20.
No non-Chinese model that fits 64 GB matches Qwen3.8 yet. The closest
candidate is Ricursive Ripple-65B Preview (US, trained from scratch, Apache
2.0, dense 65B): 14 of 20 at only 3 bits, including the first non-Chinese
5/5 run, but at 2.5 tokens/s on the M1 Max it belongs on a Mac Studio
(Q8 is 69 GB, BF16 130 GB; one 256 GB machine holds it without clustering).
webAI's TwIL-8B (11 of 20 at Q8) is notable for its size but carries a
non-commercial licence.

### 16.6 Risks

- Prefill speed on long agent contexts; set expectations per tier.
- Quality gap to frontier models; position as sufficient, not equal.
- Ollama, LM Studio and exo already cover single-machine serving; the
  difference is team management, compliance metadata, evals and
  Switchboard integrations. TensorFold Starlight is the closest match to this
  plan; see 16.9.
- Apple hardware pricing and supply, and macOS fleet management.
- Model licences vary; the catalog must enforce them.

### 16.7 Identity and the review bot as a merge gate

Today the UI uses one shared Basic Auth login, or Cloudflare Access in front
of overload; webhooks are verified with a shared secret and the GitHub App
authenticates with its private key. Security-focused buyers will want named
users and no long-lived shared secrets.

**People (OIDC login)**
- Authorization code flow with PKCE for the dashboard; `state` and `nonce`
  checked; ID tokens verified against the provider's discovery document and
  JWKS (go-oidc caches and rotates keys).
- Server-side sessions in Postgres (opaque, HttpOnly, Secure, SameSite
  cookie), so logout and revocation take effect immediately; RP-initiated
  logout where the provider supports it.
- Roles from a configurable groups claim: admins manage models, nodes,
  workflows and keys; members use models and see their own usage; viewers
  read dashboards. Unmapped users get no access by default.
- Per-user API keys for agents and CLIs, stored hashed, scoped and
  expiring, and revoked when the user is disabled. Usage and audit records
  carry the user.
- Basic Auth stays for single-user installs; Cloudflare Access stays as an
  alternative front door. A break-glass local admin is available when the
  provider is down. SCIM provisioning is a later addition.
- Tests run against Dex (an open-source OIDC provider) in the kind
  environment.

**Workloads (OIDC for automation)**
- Accept GitHub Actions OIDC tokens (issuer
  `https://token.actions.githubusercontent.com`, audience set to the
  overload URL). Policy names which `repository`, `ref`, `environment` and
  `job_workflow_ref` claims may trigger which reviews or agents, so a CI job
  can request a review or call a model without a stored secret, and a forged
  request cannot obtain an approval.
- Accept Kubernetes service-account tokens the same way for in-cluster
  callers.
- Outbound, the GitHub App still signs with its private key (GitHub Apps
  have no OIDC option); keep it in the platform's secret store and support
  rotation.

**The review bot as a merge gate**
- Overload already posts an `overload` commit status and an APPROVED or
  CHANGES_REQUESTED review. Make the status a required check in branch
  protection or rulesets; required status checks work with GitHub Apps.
  Check before relying on it whether an app's approving review counts
  toward required human approvals.
- An AI gate is an attack surface: PR text can try to talk the model into
  approving. Keep PR content untrusted (as today), require humans alongside
  the bot on sensitive paths (auth, migrations, CI configuration), allow
  bot-only approval only for low-risk paths a workflow names, and never
  fail open: a review that cannot finish, or a file that cannot be
  reviewed, must not produce a passing status.

### 16.8 Engines and model techniques to evaluate

- **TensorFold** (Apache 2.0, Zig, Metal, CUDA and HIP): a model-specific
  engine with OpenAI, Responses and Anthropic Messages endpoints, tools and
  JSON-schema output, and speculative decoding (a small drafter proposes
  tokens that the model verifies, so greedy output is unchanged). Its
  recipes report Gemma 4 31B on native Metal at about 51 tokens/s plain and
  150 to 190 with a DFlash drafter, MTP drafting for Nemotron 3.5, and
  Qwen3.8 27B about 3x faster with its drafter. It does not yet serve Flint,
  gpt-oss or Mistral models, and GGUF is not a native path. Measured on the
  M1 Max (deploy/README.md): Nemotron 3.5 MLX 4-bit ran at roughly twice
  llama.cpp's Q8 speed but found 6 of 30 planted bugs against 8 of 20 at
  Q8; the MTP drafter corrupted sampled output on the M1 (a repeated token
  until the limit) and was slower than no drafts at temperature 0; Gemma 4
  31B is refused below M5. Next step: report the M1 drafter bug upstream,
  and repeat on M5-class hardware (Gemma 4 31B with its drafter, Nemotron
  at 8 bits) before deciding whether to support it as a backend next to
  llama.cpp and MLX.
- **LLM-directed weight editing** (the MIT "Weight-Space Programming"
  report using GPT-6 Astra on Qwen3.5-0.8B): a model inspected and patched
  0.016 percent of the weights to stop a looping failure, lifting MATH-500
  from 7.9 to 48.1 percent. The editor is a hosted model, the gain was
  repairing one failure rather than adding skill, and the same attempt on
  Gemma did not help. Not actionable for code review now; revisit only for
  a specific, measurable failure (for example wrong line numbers) with the
  planted-bug eval guarding against regressions.

### 16.9 Competitors

**TensorFold Starlight** (https://github.com/ashhart/tf-starlight;
engine at https://github.com/ashhart/TensorFold and https://tensorfold.dev)
is the closest match. Reviewed November 4, 2026.

- *What it is*: "one OpenAI- and Anthropic-compatible endpoint for [an
  organisation's] private AI fleet". A stateless Go gateway and control
  plane with Postgres (`starlight`, deployed with Compose, Helm or systemd),
  a node agent that supervises TensorFold engines on macOS, Linux and
  Windows (`starlight-node`), and a desktop sign-in and tray app with a
  local endpoint for tools (`tensorfold-connect`).
- *Features*:
  - OIDC and SAML sign-on with 18 provider presets and break-glass access;
    SCIM 2.0 provisioning.
  - Projects, roles, API keys, service accounts and device sign-in for
    headless tools.
  - Limits, budgets, priorities, queueing and failover.
  - A live console: tokens/s per node, requests in flight and queued, KV
    cache load, drain and revoke controls.
  - Model catalogue, placement and rollouts (including tensor-parallel
    ranks on GPUs); configuration as code (`starlight apply -f`).
  - Prometheus metrics; mutual TLS to nodes; secrets sealed with AES-GCM;
    a hash-chained audit log; an offline mode that blocks cloud egress.
  - Brokering approved frontier providers under the same identity, limits
    and audit, and "frontier-planned local work" (plan with a frontier
    model, build locally).
- *Licence and stack*: Apache 2.0; Go (about 2 MB of source), Postgres 15+.
  The same language and database as overload.
- *Maturity*: repository created November 2, 2026, five commits, one
  contributor (Ash Hart, TensorFold's author), one star at review time. Its
  own qualification page says end-to-end tests use a fake engine and that
  real hardware, identity tenants and load still need qualifying.
- *Limits*:
  - Runs only the TensorFold engine, which serves model families one at a
    time: no Flint, gpt-oss or Mistral models, no native GGUF, and DeepSeek
    not yet in release builds.
  - No code review, GitHub integration, evaluation of models on a team's
    own code, or model provenance and licence policy found in its docs.
- *What it means for us*: their feature list is close to 16.4 and 16.7,
  so identity, budgets and a fleet console alone will not differentiate
  overload. Our edge has to be:
  1. Engine choice: llama.cpp for new architectures on day one (Flint was
     unsupported elsewhere), MLX for clusters, and TensorFold where it is
     faster.
  2. Workflows on top of the models: PR review, scheduled agents and the
     review bot as a merge gate (16.7).
  3. Evidence: the planted-bug eval as a bake-off on the customer's own
     code.
  4. Compliance metadata: model origin, base model, training-data sources
     and licence terms, enforced by policy (16.5).
  5. Switchboard as the integration layer.
  Options: compete directly; support Starlight-managed TensorFold nodes as
  one backend; or offer overload's workflows, evals and policy on top of
  Starlight. Revisit once it has real deployments, and track its commits
  and releases until then.

**Others**

| Product | Overlap | Gap relative to this plan |
|---|---|---|
| Ollama | Easy single-machine serving and model library | No team identity, budgets, fleet or workflows |
| LM Studio | Desktop serving and model management | Single machine, desktop-first |
| exo | Clustering Macs over Thunderbolt 5, dashboard, OpenAI and Claude APIs | Cluster runtime, not a team control plane |
| TensorFold engine | Fast model-specific serving with drafters | An engine; Starlight is its control plane |

Not yet reviewed: LiteLLM and other open-source gateways (team keys and
budgets in front of any backend), and hosted private-inference offerings.

### 16.10 Local multi-agent review with hosted models

Developers on several teams, mostly junior and on ordinary laptops, have
asked for the Curri Overmind PR review on their own machines. What they
expect is the Overmind experience (a cohort of specialist reviewers, a
judge, a posted review, then fixes until the review is clean) with overload
managing the agent runs locally and the models hosted. Local models are an
option for teams with a Mac Studio, not the default.

**How Overmind does it today** (PR Review Flow, read through Switchboard
and the agents repository, November 2026):

1. An entry agent calls a prepare step that returns the diff, the changed
   files and a risk tier (trivial, lite or full, from effective size and
   sensitive paths such as schema, auth and CI).
2. It launches a path-gated cohort in parallel: a main monorepo reviewer
   plus security, performance, schema, query cost, test coverage, deploy
   compatibility, docs, AGENTS.md, consolidation and Curri foot-gun
   specialists. Each is a headless Crush with its own prompt, skills and
   tool role, in its own pod.
3. A coordinator dedupes and filters the findings, a comment agent posts
   the review, and a codeowner-substitute status gates the merge. Separate
   flows (steward, review resolver, CI fixer) push fixes, which retrigger
   the review until it reports 5/5 confidence with no findings.

Observed: 5 to 10 agents and about 110 to 190 model turns per review;
completed reviews p10 4.6, p50 7 and p90 11 minutes; about $3.50 to $5.30
per review with frontier models (mostly GPT-6.1 Sol, GPT-6 Luna for light
agents).

**Overload's shape for a local run:**

- *Install and run* on the developer's machine (the existing installer and
  launchd services), against a local checkout or a PR fetched with `gh`.
- *Prepare*: diff, changed files, risk tier and specialist selection,
  ported from Overmind's prepare and path gate.
- *Specialists*: definitions (prompt, skills, tool allowlist, model,
  required or optional, which tiers) stored as overload agents. They run
  either through overload's harness or as local Crush processes started
  with a generated configuration; overload enforces a concurrency limit and
  a per-review budget.
- *Judge and output*: a coordinator agent dedupes and scores; output is a
  local report, a PR comment, or both, using the existing posting rules.
- *Fix loop (opt-in)*: a fixer agent works in a git worktree on its own
  branch, runs the repository's tests, and the review reruns until it
  reaches 5/5 or a round limit. Nothing is pushed without the developer's
  approval; a failed or unfinished review never reports success.
- *Models*: a per-agent model chain over OpenAI-compatible endpoints (the
  team's gateway such as Cloudflare AI Gateway or OpenRouter, or provider
  keys) with native Anthropic support so Claude models can think. The same
  endpoint setting can point at a local llama.cpp, MLX or Mac Studio node.
- *Credentials*: only environment-variable names are stored (as today);
  agents get scoped model access through the model proxy, never raw keys.

**Expected cost and speed with hosted models:** the laptop only runs
overload and the agent processes, so any modern machine works. A review
should take about as long as Overmind's (minutes), at roughly Overmind's
per-review model cost, billed to the team's own provider account. Quality
tracks the chosen models: on the planted-bug eval the frontier models found
19 or 20 of 20 at high effort (deploy/README.md).

**With local models instead**, measured with Flint Q8 on the M1 Max (64 GB):
prompt reading 404 tokens/s for one agent and 324 combined for eight;
generation 32 tokens/s for one and about 46 combined for four or eight,
because one GPU serves every agent. Applied to Overmind's workload that is
an estimated 20 to 60 minutes per review on a 64 GB machine (with Flint Q4
so eight 60k-token contexts fit), 1 to 2 hours on 32 GB with fewer agents
at once, not viable at 16 GB, and 1.5 to 4 hours for a review and fix loop.
Flint found 15 of 20 planted bugs against 19 to 20 for frontier models, and
long multi-agent tool use is harder for local models than that single-shot
test. Local-model reviews belong on a shared Mac Studio node (16.2), not on
junior developers' laptops.

**Risks**: per-review model cost; code is sent to model providers (use
gateways with payload logging off and zero-data-retention routes); prompt
injection from PR content into reviewers and fixers; fixer changes that
pass tests but are wrong, which is why the loop stays opt-in and
branch-only.

**Next step**: port the main reviewer, security and schema specialists and
the coordinator into overload, run them with hosted models on a dozen real
Curri PRs beside Overmind, and compare findings, wall time and cost; then
repeat with a Flint endpoint to replace the local estimates with
measurements.

### 16.11 Implementation phases

Section 16 is built in phases, one pull request each, stacked in this order.
Each phase ships tested code, CLI and UI where it applies, and updates this
plan and deploy/README.md.

1. **Model catalog and origin policy** (16.4 item 1, 16.5; done): a built-in
   catalog of open models with origin, base model, training-data sources,
   licence terms, known-good settings and measured eval results; model
   connections can name a catalog entry; an administrator policy restricts
   models by origin country and licence, enforced when a model is saved and
   when a run is queued.
2. **Runtime manager** (16.4 item 1; done): checksum-verified, resumable model
   downloads; disk accounting and cleanup; the minimum llama.cpp build per
   catalog entry checked against the installed server; llama-server launchd
   services with the catalog's settings, with start, stop and health.
3. **Native Anthropic connections** (13, 16.10 Models; done): an `anthropic`
   provider through Fantasy, with adaptive thinking and effort, so Claude
   models can think.
4. **OIDC sign-in, sessions and roles** (16.7 People): authorization code
   with PKCE, Postgres sessions, roles from a groups claim, logout, and Basic
   Auth kept as break-glass access.
5. **Per-user API keys and the model gateway** (16.4 item 2, 16.7): hashed,
   scoped, expiring keys; OpenAI and Anthropic compatible endpoints in front
   of saved models, with the origin policy, a fair per-model queue and usage
   in tokens and minutes per user.
6. **Workload identity** (16.7 Workloads): GitHub Actions and Kubernetes
   service-account tokens accepted under a claim policy, so CI can request a
   review or call the gateway without a stored secret.
7. **The review bot as a merge gate** (16.4 item 7, 16.7): the commit
   status never passes a review that did not finish or skipped files;
   protected paths that always need a human; bot approval limited to named
   low-risk paths; a documented branch-protection recipe.
8. **Model bake-offs** (16.4 item 3): eval suites built from the planted-bug
   fixtures and from commits that fixed real bugs in the team's repositories,
   run against chosen models, with detection, placement and time per model.
9. **Local multi-agent review** (16.10): risk tier and specialist selection
   by path, specialist and coordinator starter agents, a per-review budget,
   reviews of a local checkout, and an opt-in fix loop on a branch in a git
   worktree that never pushes.
10. **Node inventory and cluster control** (16.4 item 4): Macs report
    hardware, runtime versions, models and health; version drift is flagged;
    model placements across nodes are recorded and an MLX launch
    configuration is generated. Running models across a real Thunderbolt 5
    cluster still waits for the hardware spike in 16.3.
11. **Packaging** (16.4 item 5): a signed and notarized macOS package when
    signing credentials are configured, and a published control-plane
    container image.
