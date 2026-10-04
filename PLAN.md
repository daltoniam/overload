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
| `OVERLOAD_UI_USER`, `OVERLOAD_UI_PASSWORD` | Basic auth for the UI (required unless `OVERLOAD_UI_INSECURE=1`) |
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

A **model connection is not an agent**. An agent's *entry prompt* describes its
role, task and allowed context; a *review prompt* can specialize each review
pass. Keep the output JSON contract and untrusted-input boundary in a separate
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
- **Main agent**: model connection, review prompt, optional planner prompt,
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
4. **Unclaimed files** go to the main agent (unless disabled). No file is
   left unreviewed except through skip globs.
5. **Reviews**: each (agent, file) pair is today's per-file review call.
   Sub-agents see only their assigned files.
6. **Verifier** (optional): one call per sub-agent finding with that file's
   diff and content; the reply is `keep` or `drop` with a reason. It may not
   rewrite findings, so cross-run fingerprints and duplicate suppression
   still work. Dropped findings are kept on the run, marked dropped.
7. **Overload** validates every finding against the diff, removes
   duplicates, records attribution, and is the only component that posts to
   GitHub.

**Failure handling.** A failed sub-agent does not fail the review silently
or pass silently: the run completes as **degraded**, names the failed agent
and the files it did not review, and posting includes a note that the
review is partial. A failed planner falls back to globs. A failed verifier
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
1. Data model, snapshot version, findings attribution, migration of
   workflows and passes; the shared per-connection limiter.
2. Skip globs and sub-agent scopes; the run page breakdown; the preview.
3. Planner and verifier with validation and fallbacks.
4. UI for the tree (main agent, sub-agent list with scope chips, preview).

Each stage keeps `make ci`, the macOS install test and the kind sandbox
test passing, and extends the planted-bug eval: a multi-file fixture where
a file with a misleading name must still reach the right sub-agent through
the planner.

**Later, opt-in.** Run-time delegation as a main-agent mode for
tool-capable hosted models, using the same allowed sub-agents and limits.
Not part of v1.

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
                  review_prompt_revision_id, final_prompt_revision_id,
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
  modes context, plan, tools.

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
2. **Prompts**: edit named entry, review and final templates as real text;
   show variable reference, version history, preview with redacted sample
   context, and a clear warning that a prompt is not a secret store.
3. **Agents / Workflows**: configure each agent's model and independent
   entry/review/final prompt, then see ordered/parallel steps, output kind,
   caps and failure policy in a readable workflow diagram/list. Provide a
   starter single-pass PR workflow; advanced options can be progressive.
4. **Repositories**: list installed GitHub repos; each detail page shows
   enabled/dry-run state, matching event actions, bound workflow and effective
   config (workspace defaults vs repo override). Provide a test-match action
   using a sample verified event, without executing a job or posting.
5. **Schedules / Sources**: create a cron schedule with timezone and next
   three fire times, pause/resume and a dry-run `Run now`. List webhook sources
   with auth status and matching bindings. Do not imply Sentry works before an
   adapter exists.
6. **Runs**: group by source/kind/repo/schedule and show the resolved workflow
   and prompt revisions, per-agent step outcomes, deduped output and clear
   skip/failure reasons. From a failed binding, link to its configuration.

All these resources must be manageable through a local CLI (`models`,
`prompts`, `agents`, `workflows`, `bindings`, `schedules` with list/show/apply
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

Before the first release:
1. Main agents and sub-agents (7.8), in its four stages.
2. Measure `ds4-server --batched-session` with the per-connection limiter on
   the target Mac Studio (not the M1 Max used so far), with Qwen3.8 and
   DeepSeek V4 Flash, to set defaults for Parallel files.
3. Publish the first release and Homebrew formula; install on a clean Mac.
4. Run the GitHub App and posting against a real repository through a tunnel.

After:
5. Evaluation: real PRs with known bugs, scored per model and per workflow
   shape, beyond the planted-bug fixtures.
6. A reaper for sandbox claims left by a crashed worker; backup, restore and
   upgrade tests.
7. Sentry and other signed webhook sources for generic jobs.

## 13. Follow-ups (post v1, keep interfaces ready)

- **Cloud model providers**: Fantasy already supports Anthropic, OpenAI,
  Bedrock, Google and OpenRouter; add them to model profiles. Prefer an
  overload-hosted model gateway (OpenAI-compatible proxy in the control plane)
  so API keys never enter sandboxes and token usage is metered centrally.
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
  measure before making it a default.
- Whether the verifier removes more false positives than true findings on
  local models.
- Whether a hosted model proxy (keys held outside sandboxes, per-run tokens)
  should be added so hosted models can run in sandbox mode.
