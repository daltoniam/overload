# Install overload on a Mac

This guide sets up overload on one Mac (Apple Silicon) so it reviews pull
requests on your repositories, with a model running on the Mac, a hosted
model, or both. Expect about 20 minutes, plus model download time for a
local model.

You need:

- macOS on Apple Silicon. A local model needs plenty of memory (see
  [Choose a model](#2-choose-a-model)); hosted models need none.
- [Homebrew](https://brew.sh).
- A GitHub account that can install a GitHub App on the repositories you
  want reviewed.
- For webhooks: a domain on Cloudflare (free plan is fine). Without one you
  can still review pull requests from the command line.

## 1. Install overload

```sh
brew install daltoniam/tap/overload
overload install
```

or, without Homebrew:

```sh
curl -fsSL https://raw.githubusercontent.com/daltoniam/overload/main/install.sh | sh
```

`overload install` creates a private Postgres database for overload, starts
overload and Postgres as login services, and prints the UI address and a
generated password:

```
overload is running at http://127.0.0.1:8082
  user:     admin
  password: 3f9c…
```

If a model server is already running on port 8000 (DwarfStar) or 8080
(llama.cpp), it also prints the command to add it.

Open the address and sign in. Your settings live in
`~/Library/Application Support/overload/overload.env`; keep that file
private. Useful commands:

| Command | Does |
|---|---|
| `overload status` | Shows the services and whether the UI is healthy |
| `overload install` | Run again to restart with the current settings |
| `overload uninstall` | Stops the services and keeps your data (`--purge` deletes it) |

Logs are in `~/Library/Application Support/overload/logs`.

## 2. Choose a model

Overload talks to any OpenAI-compatible chat API. Pick one or more; a
workflow can mix them.

| | Local model on the Mac | Hosted model |
|---|---|---|
| Cost | Free after download | Per token |
| Speed | Minutes per file | Seconds per file |
| Privacy | Code never leaves the Mac | Code is sent to the provider |
| Needs | 64 GB+ of memory for the recommended model | An API key |

### Local: Qwen3.8 on DwarfStar (recommended)

[DwarfStar](https://dwarfstar.sh/) runs large models on Apple's GPU. Qwen3.8
Flash Next (Q2) found all five planted bugs in every test run, at about 8 to
12 minutes per changed file on an M1 Max. It needs about 44 GB of memory at
32k context, so a 64 GB Mac or larger.

```sh
git clone https://github.com/antirez/ds4 && cd ds4 && make
./download_model.sh qwen38-q2        # about 137 GB; needs the hf CLI
./ds4-server --ctx 32768 --prefill-chunk 1024 --port 8000
```

Overload does not start model servers; keep `ds4-server` running (for
example in a terminal tab), then add it to overload:

```sh
overload settings set --name qwen --url http://127.0.0.1:8000/v1 \
  --model qwen3.8-flash-next --reasoning-param reasoning_effort \
  --reasoning-effort high --max-output-tokens 28000 --default
```

### Local: a smaller model on llama.cpp

For a Mac with less memory, run a GGUF model with llama.cpp
(`brew install llama.cpp`) and at least 64k context per request:

```sh
llama-server -m model.gguf --port 8080 -ngl 99 -c 65536 --jinja
overload settings set --name local --url http://127.0.0.1:8080/v1 \
  --model local --reasoning-param chat_template --reasoning-effort medium
```

Smaller models find fewer bugs; see the measurements in
[deploy/README.md](../deploy/README.md#measurements).

### Hosted: OpenAI, OpenRouter, Cloudflare AI Gateway, …

Any provider with an OpenAI-compatible API works: OpenAI, OpenRouter,
Together, Groq, Fireworks, Cloudflare AI Gateway, LiteLLM or vLLM. Claude
and Gemini work through a gateway that offers an OpenAI-compatible API (for
example OpenRouter or Cloudflare AI Gateway).

Overload never stores API keys. Put the key in `overload.env` under a name
starting with `OVERLOAD_MODEL_`, and give overload only the name:

```sh
echo 'OVERLOAD_MODEL_OPENAI_KEY=sk-…' >> ~/Library/Application\ Support/overload/overload.env
overload install        # restarts overload so it reads the key
overload settings set --name openai --connection-kind hosted \
  --url https://api.openai.com/v1 --model <model id> \
  --api-key-env OVERLOAD_MODEL_OPENAI_KEY \
  --reasoning-param reasoning_effort --reasoning-effort medium --concurrency 8
```

Use any model ID your account can call. For OpenRouter use
`--url https://openrouter.ai/api/v1` with its model IDs (for example
`anthropic/…` or `google/…`). `--concurrency` sets how many files are
reviewed at once; hosted APIs handle 8 easily, local servers usually 1.

You can do all of this on the **Models** page instead of the command line.

## 3. Create agents and a workflow

An **agent** is a model plus its instructions. A **workflow** has one main
agent and up to eight sub-agents, each limited to the files it is for.

1. **Agents → New agent.** Name it `reviewer`, pick your model, click
   **Start from: General code review** and save.
2. Optional: add focused agents, for example `security` on a hosted model
   with instructions about authentication and data handling.
3. **Workflows → New workflow.** Choose `reviewer` as the main agent. Add
   sub-agents with path globs (for example `security` with
   `**/auth/**`), and skip paths such as `*.lock` and `vendor/**`.
4. Use **Preview routing** on the workflow to paste a list of changed files
   and see which agent reviews each one, before any model runs.

Try it on a real pull request without GitHub setup:

```sh
overload review --repo owner/name --pr 123 --workflow my-workflow
```

This uses `gh auth token` or `GITHUB_TOKEN`, prints the findings and saves
the run in the UI. Nothing is posted.

## 4. Receive pull request webhooks

GitHub needs a public https address to send pull request events to your
Mac. Overload sets one up with a Cloudflare Tunnel; only the webhook path
is exposed, never the UI.

1. `brew install cloudflared`
2. **GitHub → Set up Cloudflare Tunnel**, **Log in to Cloudflare** (pick
   your domain in the browser), enter a hostname such as
   `overload.example.com` and click **Create tunnel**.

Details and a manual alternative: [cloudflare-tunnel.md](cloudflare-tunnel.md).

## 5. Create the GitHub App

1. **GitHub** page: enter an App name; the webhook URL is filled in from the
   tunnel. Click **Create GitHub App** and confirm on GitHub. Overload stores
   the App's credentials.
2. **Install on repositories** and choose the repositories to review. They
   appear under **Repositories**, disabled and in dry run.
3. Optional: under **App logo**, download the overload logo and upload it in
   the App's settings, or GitHub shows your profile picture on reviews.

## 6. Turn on reviews

1. **Repositories →** your repository: check **Enable processing**.
2. Under **PR event bindings**, add **Opened** and **Updated** with your
   workflow.
3. Open or push to a pull request. It appears on **Webhooks** within seconds
   and on **Runs** as it is reviewed.

Reviews are dry runs until you allow posting. To post findings as review
comments:

1. On the repository, check **Post reviews as GitHub comments**.
2. Add `OVERLOAD_ENABLE_POSTING=1` to `overload.env` and run
   `overload install` to restart.

Overload posts one review per run with inline comments, never more than the
workflow's finding limit (10 by default), and does not repeat findings it
already posted on the pull request.

## Troubleshooting

| Symptom | Check |
|---|---|
| Nothing on **Webhooks** after a push | The tunnel page shows the service running; the App's settings → Advanced lists deliveries and their responses |
| Delivery skipped "no matching workflow binding" | The repository needs a binding for that PR action |
| Run failed "model call timed out" | The model server is down or hung; restart it |
| Run is partial | A sub-agent failed or a file was too large; the run page names which |
| "Unknown host" in the browser | Use `http://127.0.0.1:8082`, or set `OVERLOAD_BASE_URL` to the address you use |
