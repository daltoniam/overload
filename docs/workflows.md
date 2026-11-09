# Agents and workflows

An **agent** is a model plus its instructions. A **workflow** decides which
agents review which files of a pull request. Both can be edited in the UI or
applied as JSON from the CLI (`overload agents|workflows|bindings|
repositories|schedules list|apply`), so a coding agent can configure overload
without the UI.

## Agents

```json
{"name": "security", "model": "qwen", "prompt": "Look for injection, authz and secrets bugs.", "enabled": true}
```

`prompt` holds the agent's instructions. In the UI, **Start from** fills in a
built-in prompt you can edit. Changed instructions are saved as new
versions, and each run records the exact text it used.

## Pull request workflows

The first agent of a PR workflow is its **main agent**; the rest (up to
eight) are **sub-agents**.

| Field | What it does |
| --- | --- |
| `skip_paths` | Drops files such as `*.lock` or `vendor/**` from every review. |
| `scopes` | Limits a sub-agent to matching files and, optionally, its number of findings. |
| scope `mode` | `globs` (default), `always` (only its paths), `planned` (only files the planner assigns) or `whole_pr` (its files reviewed together; see [Whole pull request context](#whole-pull-request-context)). |
| scope `description` | Tells the planner what the sub-agent is for. |
| `main_reviews` | `"unclaimed"` leaves files a sub-agent claimed to that sub-agent; otherwise the main agent reviews every file. |
| `max_file_reviews` | Caps the number of file reviews per run. |
| `max_findings` | Caps the findings posted per review (default 10, at most 50), most severe first. The rest stay on the run as dropped. |
| `planner_prompt` | One call on the main agent's model that can send extra files to sub-agents whatever their path. Empty means no planner. |
| `verifier_prompt` | One call per sub-agent finding that keeps or drops it before posting. Empty means no verifier. |
| `max_file_chars` | Characters of a file's diff and content sent in one review (8,000–400,000). Files whose diff alone is larger are skipped as too large and the review is marked partial. Default: 160,000 when every agent uses a hosted model, 32,000 when any agent uses a local model. |
| `review_decision` | `comment` (default) posts findings as comments. `request_changes` requests changes when a finding is at or above `block_severity`. `approve` does that and approves a complete review with nothing blocking. |
| `block_severity` | `critical`, `high` (default), `medium` or `low`: the lowest severity that requests changes. |
| `max_run_cost_usd` | The most one review may spend on priced models. A review that reaches it starts no more model calls and completes as partial. See [Costs and budgets](#costs-and-budgets). |
| `context` | A shared pull request bundle for every review call; see [Whole pull request context](#whole-pull-request-context). |
| `tiers` | Skip trivial pull requests and review small ones with fewer agents; see [Tiers](#tiers). |
| `rereview` | Incremental, debounced and limited re-reviews; see [Re-reviews](#re-reviews). |

With a review decision, overload withdraws (dismisses) its own earlier
request for changes once a later review of the pull request finds nothing
blocking, so a fixed pull request is not left blocked. A blocking issue
still present from an earlier commit keeps counting even though it is not
commented again. A partial review (a sub-agent failed) never approves.
Approvals from GitHub Apps count toward required reviews only if branch
protection accepts them.

Planner assignments only add reviews: the main agent still reviews every
file no sub-agent's paths matched. The verifier never drops critical or
security findings, because it reads untrusted pull request text.

## Commit status and reviewing on demand

Overload sets an `overload` commit status on the pull request's head:
pending while it reviews, then success, or failure when the review
requests changes (and error if the review fails). Dry-run repositories get
no status. Make it a required check in branch protection to hold merges
until overload has reviewed. The GitHub App needs **Commit statuses: Read
and write**; Apps created before this was added must grant it in their
settings, and each installation must accept it.

GitHub does not retry webhooks a GitHub App failed to receive. Every five
minutes overload asks GitHub for deliveries from the last day that never
reached it (no response, a timeout or a server error, for example during
a restart or a tunnel outage) and has GitHub send them again, at most 20
at a time. Set `OVERLOAD_WEBHOOK_CATCHUP=0` to turn this off.

Pull requests opened before overload was watching (or that you want
reviewed again) can be queued by hand: **Review now** on the repository's
page, or `overload review --repo owner/name --pr 123 --queue`. It runs like
a newly opened pull request: the repository's `opened` binding and its
posting settings. A head that already has a review is reviewed again only
when you ask for it (the checkbox, **Review the pull request again** on a
run's page, or `--again`), for example after changing the workflow.

## Costs and budgets

Give each model its prices (US dollars per million tokens) on the Models
page or with `overload settings set ... --price-input 2
--price-cached-input 0.10 --price-output 10`. Cached input and cache
writes default to the input price; `--long-context-tokens 272000
--long-context-input-multiplier 2 --long-context-output-multiplier 1.5`
prices calls with a long prompt higher. Prices are pinned with the
workflow when a run is queued, so editing them later does not reprice old
runs. A model without prices shows as "unpriced", never as $0.

Every run records its tokens (input, cached input, cache writes, output)
and cost per agent, planner and verifier. The run page, the dashboard
(today, this month, average per run) and

```sh
overload runs cost --since 2026-11-01 [--repo owner/name] [--json]
```

report spend, with the average and p90 per run.

Budgets cap spend per calendar month and per day (UTC), globally and per
repository:

```sh
overload budget set --monthly 3000 --daily 200
overload budget set --repo owner/name --monthly 500
overload budget show
```

or the Models page, or `OVERLOAD_BUDGET_MONTHLY_USD` and
`OVERLOAD_BUDGET_DAILY_USD` (which take precedence). When a review is
queued, overload adds this period's spend, what queued and running
reviews are expected to cost (their workflow's `max_run_cost_usd`, or the
average of its last 20 runs) and this review's expected cost; if that
passes a cap, the delivery is skipped as `budget_exhausted` (shown on
Webhooks) and the head gets an `overload` commit status of `success`
("Overload skipped: monthly review budget reached"), so a required check
does not block merges. `--skip-status error` blocks them instead. As a
backstop, the model proxy meters every sandboxed call and answers 429
once a global cap is passed by more than 5%. The dashboard and the log
warn at 80% and 100% of a cap.

## Whole pull request context

A `context` block (it may be empty: `"context": {}`) adds a bundle to the
start of every review call in a run: the pull request's title and
description (untrusted, stripped of links, HTML and mentions), its
changed files with line counts, and a **contracts index**: each function,
method, class, type, GraphQL field or exported constant the diff removes,
renames or changes, with up to `max_callers` (default 8) places the head
still uses it, uses outside the diff first. The index is built in the
sandbox from the checked-out head and the diff, without a model or the
network, for Go, Python, TypeScript/JavaScript and GraphQL, and is capped
at `max_index_chars` (default 10,000). Because the bundle is the same
for every call in the run, OpenAI caches it automatically and Anthropic
calls mark it for caching; the run's cached input tokens show the hit
rate.

A sub-agent with scope mode `whole_pr` reviews every file its paths match
(all files without paths) in one call with the bundle, so it can catch
bugs that span files: a renamed function still called elsewhere, a
changed contract, a migration and the code that reads it. It never takes
files away from the main agent. When the diff is over `whole_pr_chars`
(default 200,000), related files (same directory, or linked by the
contracts index) are reviewed together in a few calls, and a file too
large on its own is left out and the review marked partial. With
`"repo_tools": true`, whole-PR agents can also read files, search and
list directories of the checked-out head (read-only, never the network),
for at most `max_tool_steps` model calls per review call (default 12).

```json
"scopes": {"correctness": {"mode": "whole_pr"}},
"context": {"max_index_chars": 12000, "repo_tools": true}
```

## Tiers

Tiers sort pull requests by changed lines (added plus removed) and files,
after `skip_paths`, without a model:

```json
"tiers": {
  "sensitive_paths": ["**/migrations/**", "**/auth/**", "**/*.tf", ".github/workflows/**"],
  "trivial": {"max_lines": 10, "max_files": 3, "action": "skip"},
  "lite": {"max_lines": 100, "max_files": 20, "agents": ["main"]}
}
```

A file matching `sensitive_paths` always gets the full workflow. A
trivial pull request is skipped (commit status `success`, "Overload
skipped: trivial change") or, with `agents` instead of `action`, reviewed
by those agents only; a lite one is reviewed by its `agents` only
(`"main"` is the main agent, which must be included). The tier is chosen
before the head is downloaded, and the tier and reason are recorded on
the run and shown in previews.

## Re-reviews

A `rereview` block (it may be empty) changes how pushes to a pull request
overload already reviewed are handled:

| Field | Default | What it does |
| --- | --- | --- |
| `incremental` | `true` | Review only files whose change differs from the last completed review with the same workflow revision (compared by patch content, so rebases and merges from the base branch do not count), plus files the contracts index links to them. The model sees the pull request's open findings on those files with people's replies, and marks each `resolved` or `still_present`. A fixed finding no longer counts toward the review decision; findings on unchanged files and still-present ones keep counting. |
| `debounce_minutes` | `5` | Holds a review of a new push; another push within that time replaces it. |
| `max_reviews_per_pr` | `3` | Webhook reviews per pull request (0 for no limit). Later pushes get a `success` status saying so. |
| `resolve_threads` | `false` | Resolve overload's GitHub review thread when a re-review finds its finding fixed. |

"Review now", `overload review --queue` and a comment of
`/overload review` from someone with write access to the repository
always review, whatever the limit. Organization membership alone is not
enough: overload checks the commenter's repository permission (write,
maintain or admin) before queuing the review. GitHub Apps created
before this need the **Issue comment** event enabled (App settings,
Permissions & events) for the comment to reach overload.

## Evaluate workflows on past pull requests

```sh
overload eval prs --repo owner/name --prs 101,102,110-120 \
  --workflows sol-v1,sonnet-v1 --labels bugs.json --out results.json
```

runs every workflow on every pull request's current head as a dry run on
the overload server (the same worker and sandbox as a webhook review;
nothing is posted) and writes JSON plus a CSV summary: findings per pull
request, cost and time at p50 and p90, degraded runs, and, with a labels
file of known bugs (`[{"pr": 101, "path": "a.py", "line": 12, "window":
3, "keywords": ["null"]}]`), the share each workflow caught. Evaluation
works on GitHub repositories overload already knows; heads are read
through the GitHub App, or a `GITHUB_TOKEN`/`gh` token for repositories
without an installation.

Each review also records how long fetching, uploading and extracting the
head took; `overload runs transfer` reports p50 and p90.
`OVERLOAD_ARCHIVE_CACHE_DIR` (and `OVERLOAD_ARCHIVE_CACHE_MB`, default
2048) keeps head archives on disk so reviews of the same head download it
once.

## Preview before you run

**Preview routing** on the workflow page, or

```sh
overload workflows preview NAME < files.txt
```

shows which agent would review each file, how many model calls that takes,
roughly how long and what it costs (at 12,000 input and 2,000 output
tokens per call), and the tier, without calling a model.

## Scheduled jobs

Schedules run a scheduled-prompt workflow on a cron schedule with JSON
input; see `overload schedules list|apply`, and `overload schedules run
NAME` (or **Run now** on the schedule's page) to run one immediately.

Scheduled agents can call tools on MCP servers such as Switchboard; see
[tools.md](tools.md). A scheduled workflow sets limits for those agents:

| Field | What it does |
| --- | --- |
| `max_steps` | Model calls per agent (default 40, at most 200). On the last step the agent loses its tools and writes its report. |
| `timeout_minutes` | Minutes per agent (default 30, at most 240). |
