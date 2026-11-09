# GitLab and Forgejo

Overload reviews GitLab merge requests and Forgejo (or Gitea) pull requests
the same way it reviews GitHub pull requests: the same agents, workflows,
routing, verification, inline comments and commit status. It works with
gitlab.com, codeberg.org and self-hosted servers.

Each server is a **git host connection**: a name, a kind (`gitlab` or
`forgejo`), the server's address and an access token. Overload acts as the
token's account, so use a dedicated bot account or a project or group
access token rather than a person's token.

## 1. Create a token

**GitLab**: a project or group access token (Settings → Access tokens), or
a personal access token of a bot user, with:

- Scope `api`.
- Role **Developer** to read code, comment and set commit statuses.
  **Maintainer** also lets overload create the webhook for you and approve
  merge requests when the account is an eligible approver.

**Forgejo / Gitea**: an access token of a bot user (Settings →
Applications) with **read and write** access to *repository* and *issue*,
and read access to *user*. The account needs write access to the
repositories. Repository **admin** also lets overload create the webhook
for you and withdraw its own earlier requests for changes.

## 2. Add the connection

In the UI, open **Git hosts** (`/setup/forges`), choose the kind, and enter
a name, the base URL (`https://gitlab.com`, `https://codeberg.org`, or your
server, including a path prefix if GitLab is served under one) and the
token. Overload checks the token against the server before saving it.

From the CLI, keep the token in an environment variable:

```sh
export GITLAB_TOKEN=glpat-...
overload forges apply - <<'JSON'
{"name": "gitlab", "kind": "gitlab", "base_url": "https://gitlab.example.com", "token_env": "GITLAB_TOKEN"}
JSON
overload forges list
```

The token and an overload-generated webhook secret are stored in Postgres
(table `forge_connections`), like the GitHub App's private key, so protect
database backups. Neither is shown in the UI. A new connection gets copies
of GitHub's catch-all pull request bindings, so its repositories use the
same workflows until you change them.

## 3. Add repositories

On **Repositories → New repository**, choose the host and enter the
repository path: `owner/name` on Forgejo, `group/name` or
`group/subgroup/name` on GitLab. Leave **create the webhook** checked to add
it with the connection's token, or from the CLI:

```sh
overload repositories apply - <<'JSON'
{"name": "platform/backend/api", "forge": "gitlab", "enabled": true, "dry_run": true}
JSON
OVERLOAD_BASE_URL=https://overload.example.com overload forges webhook gitlab platform/backend/api
```

Webhooks need overload's public `https://` address: set `OVERLOAD_BASE_URL`,
or on a Mac use the [Cloudflare Tunnel](cloudflare-tunnel.md) setup, which
forwards the GitLab and Forgejo webhook paths too.

### Adding the webhook by hand

The **Git hosts** page shows each connection's webhook URL
(`/webhooks/gitlab/N` or `/webhooks/forgejo/N`). Print the secret with
`overload forges secret NAME`, then:

- **GitLab**: Settings → Webhooks. URL as shown, **Secret token** the
  secret, trigger **Merge request events** only.
- **Forgejo / Gitea**: Settings → Webhooks → Add webhook → Forgejo (or
  Gitea). URL as shown, content type `application/json`, **Secret** the
  secret, trigger **Custom events → Pull request** and **Pull request
  synchronized**.

Deliveries without the secret are refused with 401.

## What overload does on each host

| | GitHub | GitLab | Forgejo / Gitea |
| --- | --- | --- | --- |
| Starts a review | opened, new commits, reopened, ready for review | opened, new commits, reopened, marked ready (draft removed) | opened, new commits, reopened, `WIP:` prefix removed |
| Inline comments | one review with comments | a summary note, then one discussion per finding on its line | one review with comments |
| Findings that cannot go on a line | refused with the review | listed in a follow-up note | refused with the review |
| Approve | review approval | approves the merge request (if the account may) | review approval |
| Request changes | review requesting changes, dismissed when fixed | failed commit status (GitLab's API has no request for changes); withdraws overload's approval | review requesting changes, dismissed when fixed (needs admin) |
| Commit status | `overload` status | `overload` external status | `overload` status |

Repositories are matched by the host's repository (project) ID after the
first delivery, so a renamed repository keeps its settings. Draft merge
requests and closed ones are not reviewed; **Review now** on a repository's
page and `overload review --forge NAME --repo PATH --pr N` work as on
GitHub.

GitLab needs version 15.7 or later (for the merge request diffs API).
GitLab's merge request webhook carries no base commit, so overload reads it
from the merge request when it reviews.

## Limits

- Each connection uses one token for all of its repositories. Use separate
  connections for separate GitLab groups or Forgejo organizations that need
  different accounts.
- Tested against fake GitLab and Forgejo servers that follow their API
  documentation; not yet run against real servers.
