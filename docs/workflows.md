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
| scope `mode` | `globs` (default), `always` (only its paths) or `planned` (only files the planner assigns). |
| scope `description` | Tells the planner what the sub-agent is for. |
| `main_reviews` | `"unclaimed"` leaves files a sub-agent claimed to that sub-agent; otherwise the main agent reviews every file. |
| `max_file_reviews` | Caps the number of file reviews per run. |
| `max_findings` | Caps the findings posted per review (default 10, at most 50), most severe first. The rest stay on the run as dropped. |
| `planner_prompt` | One call on the main agent's model that can send extra files to sub-agents whatever their path. Empty means no planner. |
| `verifier_prompt` | One call per sub-agent finding that keeps or drops it before posting. Empty means no verifier. |
| `review_decision` | `comment` (default) posts findings as comments. `request_changes` requests changes when a finding is at or above `block_severity`. `approve` does that and approves a complete review with nothing blocking. |
| `block_severity` | `critical`, `high` (default), `medium` or `low`: the lowest severity that requests changes. |

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

Pull requests opened before overload was watching (or that you want
reviewed again) can be queued by hand: **Review now** on the repository's
page, or `overload review --repo owner/name --pr 123 --queue`. It runs like
a newly opened pull request: the repository's `opened` binding, its posting
settings, and no second review of a head that already has one.

## Preview before you run

**Preview routing** on the workflow page, or

```sh
overload workflows preview NAME < files.txt
```

shows which agent would review each file, how many model calls that takes
and roughly how long, without calling a model.

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
