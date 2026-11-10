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
launchd agents (model services included) and keeps the data; `--purge`
deletes the install directory too, including downloaded model weights unless
`OVERLOAD_MODELS_DIR` points elsewhere. `--home`, `--label` and `--port`
allow side-by-side installs.

`make install-test` (and the macOS CI job) installs under a throwaway label
and port and checks login, `install-check`, a saved model surviving restarts
of both services, reinstall without new credentials, uninstall and reinstall
keeping data, and `--purge` cleanup.

The UI only answers to `127.0.0.1`, `localhost` and the host in
`OVERLOAD_BASE_URL`; set that to the address you use if it differs. This
blocks DNS-rebinding pages from reaching a local UI.

## Models

Any OpenAI-compatible `/v1/chat/completions` server works, as does
Anthropic's Messages API (below). Each model connection has:

| Setting | CLI flag | Meaning |
|---|---|---|
| Provider API | `--provider openaicompat\|anthropic` | OpenAI-compatible (default) or the Anthropic Messages API |
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
| `reasoning_effort` | top-level `reasoning_effort=<effort>`; for Anthropic, `output_config.effort=<effort>` with adaptive thinking | DwarfStar `ds4-server`, OpenAI-style APIs, Claude |

If a call runs out of tokens while thinking, that file is retried once at
`medium` and the run records `effort_fallbacks`. Settings are pinned into
each run, so editing a model does not change queued reviews. Only the name
of an API key's environment variable is stored, never the key, and names of
overload's own secrets (`DATABASE_URL`, `OVERLOAD_*`, `GITHUB_*`, `PG*`) are
refused.

### Anthropic models

Claude models think only through Anthropic's Messages API; through an
OpenAI-compatible endpoint (including Cloudflare AI Gateway's) they run
without thinking, and Sonnet 5.5 found 9 of 20 planted bugs that way
against 14 with effort high and adaptive thinking. Add them with the
Anthropic provider:

```sh
overload settings set --name opus --provider anthropic --connection-kind hosted \
  --url https://api.anthropic.com --model claude-opus-5-5 \
  --api-key-env OVERLOAD_MODEL_ANTHROPIC \
  --reasoning-param reasoning_effort --reasoning-effort high
```

The URL has no `/v1` (a URL ending in `/v1` or `/v1/messages` is
refused); a gateway's Anthropic endpoint works too. The effort (low,
medium, high, xhigh or max) is sent as `output_config.effort` with adaptive
thinking; `--reasoning-param none` leaves both to the model's defaults.
`--catalog claude-opus-5.5` fills the provider and effort; with `--provider
openaicompat` it leaves the effort unset, since OpenAI-compatible endpoints
ignore it for Claude. Requests stream, so long thinking is bounded only by
the call timeout (local 90 minutes, hosted 15) and output limits above
about 21,000 tokens work.

The key is sent as `x-api-key`, and redirects are never followed, so the
key cannot be forwarded to another host. Requests carry only the headers
the Messages API needs plus the connection's own, so Anthropic
credentials, profile settings (such as a workspace ID) and
`ANTHROPIC_CUSTOM_HEADERS` from overload's own environment are never sent;
a connection without `--api-key-env` sends no key. In sandbox mode the
model proxy adds the key, as for OpenAI-compatible models, and forwards
only the `anthropic-version` header from the sandbox, so a sandbox cannot
turn on beta features with the control plane's key.

### Catalog and model policy

Overload ships a catalog of most of the models measured in this file and
other open models a team might run: publisher and country, base model and
training-data sources, licence terms, hardware tier, known-good settings and
planted-bug eval results. See it under Models, Catalog and policy, or with
`overload catalog list` and `overload catalog show ID`. Fields the catalog
has not recorded (many licences, some base models) are shown as such rather
than guessed.

Name a model connection's catalog entry (`--catalog ID`, or the Catalog
entry field) so the model policy can check it. `overload settings set
--catalog recursive-flint --name flint --url http://127.0.0.1:8080/v1` fills
the model ID and reasoning settings from the entry; flags you pass win. A
connection that names no entry is matched by its model ID when that equals
an entry's ID or served name (`flint`, `qwen3.8-flash-next`), and a
connection whose model ID belongs to a different entry than the one it names
is refused.

The model policy restricts which models may review code. With no policy
every model is allowed. For example, to allow only US, Canadian and French
publishers, apply that to base models and training data too, and require a
licence that permits commercial use:

```sh
overload policy set --allow-origins US,CA,FR --check-lineage \
  --require-commercial-use --annual-revenue-usd 25000000
overload policy show    # the policy and any saved models it refuses
```

- Models whose publisher country is not recorded are refused when origins
  are allowlisted. With `--check-lineage` and an allowlist, open models
  whose base model is not recorded (Gemma, Granite, North and others) are
  refused until you confirm it with `--lineage-reviewed ID`; hosted models
  are checked by their provider's country only. Training-data sources are
  recorded only where a model card names them.
- Licences the catalog has not recorded count as not permitting commercial
  use; after reading a model's terms, list it with `--licence-reviewed ID`
  (this also covers a separately purchased licence).
- Connections that match no catalog entry are refused under an allowlist,
  the licence rule or `--refuse-uncataloged`. A policy that only blocks
  countries lets them through, so use an allowlist to fail closed.
- Every `policy set` replaces the whole policy, and `policy set` with no
  flags clears it.

A refused model cannot be saved. A workflow whose agents use one stops
queueing runs: the webhook records why it was skipped, a due schedule
records one failed run with the reason and is paused (it can be saved while
paused, and enabled again once the policy allows its models; a schedule
whose workflow is unavailable for another reason, such as a disabled agent,
is paused the same way), and
`overload review` fails with the reason.
A run queued before the policy changed fails with `model_policy` when it
starts, rather than reviewing with the refused model; GitHub only sees
"Review refused by the model policy".

The policy trusts the catalog entry a connection names unless its model ID
is a different entry's. It keeps reviews on approved models; it is not a
control against someone who can edit model connections and the policy.

### Downloading and serving models

`overload models` manages llama.cpp models from the catalog (see Catalog and
model policy above), so nothing needs to be run by hand. Downloads work on
macOS and Linux; `serve` uses launchd and needs macOS.

```sh
overload models pull recursive-flint --quant Q4_K_M   # resumable, SHA-256 verified
overload models check recursive-flint                 # is llama-server new enough?
overload models serve recursive-flint --port 8080 --connect flint
overload models status                                # services and /health
overload models list                                  # downloads, disk used and free
overload models stop recursive-flint
overload models rm recursive-flint                    # delete the weights
overload models clean                                 # delete partial and replaced downloads
```

- `pull` downloads from the catalog's Hugging Face repository (or
  `--repository OWNER/NAME`) at the commit its main branch points to, into
  `models/<owner>/<name>/<commit>/` next to the install's configuration;
  an update downloads into a new commit directory (files that did not
  change are hard-linked where the volume supports it and downloaded
  again otherwise, and an unfinished download carries over) and switches
  over only once every file has verified, so a failed update leaves the
  previous download usable. `--dir` or
  `OVERLOAD_MODELS_DIR` changes the directory; put `OVERLOAD_MODELS_DIR` in
  the configuration file so every command and the catalog page use the same
  one. An interrupted or stalled download resumes where it stopped. Every
  file is checked against the SHA-256 Hugging Face publishes for it; a
  mismatch deletes the partial file. A download that would leave less than
  2 GB free is refused. Gated models need `HF_TOKEN` or a token saved by
  `hf auth login`; the token is sent only to Hugging Face and its
  subdomains, never on redirects elsewhere.
- `check` and `serve` compare `llama-server --version` with the catalog's
  minimum build (Flint needs b11801) and say how to upgrade; a build that
  reports no number (built from source without git) needs
  `--skip-version-check` on `serve`. They use `--llama-server`,
  `OVERLOAD_LLAMA_SERVER` or `llama-server` on PATH.
- `serve` registers a launchd agent (`dev.overload.model.<id>`) that runs
  llama-server on 127.0.0.1 with the catalog's arguments, starts at login
  and restarts if it exits. It refuses a port something else uses, waits
  until `/health` reports the model loaded (printing the log if the server
  never answers), and checks the server lists the model. `--parallel N`
  gives N request slots, each with the catalog's full context.
  `--connect NAME` also saves a model connection with the catalog entry and
  settings; without it the command to add one is printed. Logs are in the
  install's `logs/model-<id>.log`. `overload uninstall` removes model
  services too. If the model never loads, `serve` prints the end of the
  log and leaves the service registered; fix the cause and serve again, or
  remove it with `stop`.
- `rm` refuses a model that is being served, and keeps files another
  downloaded entry uses. Pulling a model again from another repository
  deletes the old files, unless the model is being served from them; then
  they are kept until `serve` loads the new ones, and `clean` deletes them
  afterwards.
- The catalog page for a model shows what this Mac has downloaded and
  serves.

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

[Recursive Flint](https://huggingface.co/RecursiveSI/flint) (40B-A2.5B MoE,
Apache 2.0) runs from the
[community GGUF](https://huggingface.co/lmstudio-community/flint-GGUF)
(Q4_K_M is 24 GB, Q6_K 33 GB, Q8_0 42 GB). It needs llama.cpp build b11801
or newer; Homebrew's llama.cpp 0.7.0 (build 11669) fails with `unknown model
architecture: 'flint'`, so use a
[prebuilt release](https://github.com/ggml-org/llama.cpp/releases) until the
formula catches up. Its native context is 262,144 tokens (no RoPE scaling,
so 1M is not available), and llama.cpp holds the full window in about 7 GiB
of KV cache because only 14 of its 49 layers use full attention:

```sh
llama-server -hf lmstudio-community/flint-GGUF:Q4_K_M --port 8080 -ngl 99 \
  -c 262144 --jinja --reasoning-format deepseek
overload settings set --name flint --url http://127.0.0.1:8080/v1 \
  --model flint --reasoning-param none
```

Use `:Q8_0` for the 8-bit weights. A review prompt is about 11k tokens, so
the large window matters for agents and big contexts rather than per-file
reviews. Long prompts are slow on an M1 Max: a 205k-token prompt took about
55 minutes to process (100 tokens/s at first, 36 by the end) and generation
fell to 8 tokens/s, though Flint still retrieved a fact planted two-thirds
of the way in.

Flint always thinks and its chat template takes no effort setting, so use
`--reasoning-param none`; the out-of-tokens retry at `medium` does not
shorten its thinking, so raise `--max-output-tokens` rather than lowering
effort if files run out. Servers that leave its `<think>...</think>` block
in the answer instead of splitting it out (no `--reasoning-format`) still
work: overload drops the block before reading the review.

Liquid AI's models
([LFM2.5-24B-A2B](https://huggingface.co/LiquidAI/LFM2.5-24B-A2B-GGUF),
[LFM3-1.7B](https://huggingface.co/LiquidAI/LFM3-1.7B-GGUF)) run the same
way with `--reasoning-param none`, because their templates take no effort
setting; LFM3 needs llama.cpp b11960 or newer. Their GGUFs carry Liquid's
recommended sampling (temperature 0.1), which llama-server applies. They are
under the LFM Open License, which allows free commercial use only below
$10M in annual revenue.

### Measurements

A 92-line Go file with five planted bugs (`eval/fixtures`), run
through the normal review path on an M1 Max with 64 GB. Rerun with
`OVERLOAD_PLANTED_EVAL=1 go test -run TestPlantedBugEval -v ./harness/` and
the `OVERLOAD_EVAL_*` variables described in the test.

| Model | Origin | Planted bugs found | Output tokens | Time per file |
|---|---|---|---|---|
| Qwen3.8 Flash Next Q2, ds4, `reasoning_effort:high` | China | 5/5 in 4 of 4 runs | 7.5k to 13.5k | 8 to 12 min |
| Bonsai 2 27B, llama.cpp, `xhigh` | PrismML, built on Qwen3.8-27B (China) | 4/5 in 2 of 2 runs | 11k to 15k | 15 to 19 min |
| Bonsai 2 27B, llama.cpp, `medium` | PrismML, built on Qwen3.8-27B (China) | 3/5 in 2 of 2 runs | 2.7k to 3.5k | 3.5 to 4.5 min |
| Recursive Flint Q4_K_M, llama.cpp b11803, 64k context | US | 4/5 in 1 of 4 runs, 3/5 in 3 | 12k to 17k | 5.5 to 7.5 min |
| Recursive Flint Q4_K_M, llama.cpp b11803, 256k context | US | 4/5 in 2 of 4 runs, 3/5 in 2 | 7.9k to 16k | 4 to 7.5 min |
| Recursive Flint Q8_0, llama.cpp b11803, 256k context | US | 4/5 in 3 of 4 runs, 3/5 in 1 | 9.6k to 17.5k | 5.5 to 10.5 min |
| Ricursive Ripple-65B Preview IQ3_XXS (3-bit), llama.cpp master (67107ac), 64k context | US | 5/5 in 1 of 4 runs, 4/5 in 2, 1/5 in 1 | 5.2k to 10.5k | 36 to 72 min (2.5 tokens/s, swapping) |
| webAI TwIL-8B Q8_0 "verbose", llama.cpp b12144, 128k context | US (Granite 4.2 8B base); non-commercial licence | 4/5 in 1 of 4 runs, 3/5 in 1, 2/5 in 2 | 5.7k to 8.9k | 8.5 to 14 min |
| webAI TwIL-8B Q4_K_M "balanced", llama.cpp b12144, 64k context | US; non-commercial licence | 2/5 in 2 of 4 runs, 0/5 in 1, invalid output in 1 | 6.4k to 22.7k | 8 to 29 min |
| Gemma 4 31B Q8_0, llama.cpp b12058, 64k context | US | 4/5 in 1 of 4 runs, 3/5 in 2, 2/5 in 1 | 2.9k to 6.3k | 16 to 34 min (swapping) |
| Cohere North Mini Code 30B-A3B Q8_0, llama.cpp b12058, 256k context | Canada | 4/5 in 1 of 4 runs, 3/5 in 2, 2/5 in 1 | 11.6k to 20.6k | 12 to 24 min |
| NVIDIA Nemotron 3.5 Lightning 30B-A3B Q8_0, llama.cpp b12058, 256k context | US (see note) | 3/5 in 2 of 4 runs, 2/5 in 1, 0/5 in 1 | 10k to 26k | 9.5 to 25 min |
| NVIDIA Nemotron 3.5 Lightning 30B-A3B MLX 4-bit, TensorFold 1.2.2, drafts off, 256k context | US (see note) | 3/5 in 1 of 6 runs, 1/5 in 3, 0/5 in 2 | 7.4k to 23.6k | 3 to 10.5 min |
| IBM Granite 4.2 30B Q8_0, llama.cpp b12058, 64k context, q8 KV cache | US | 2/5 in the one run that finished | 10k | 64 min (swapping) |
| LFM2.5-24B-A2B Q8_0, llama.cpp b12058, 128k context | US | 3/5 in 1 of 4 runs, 2/5 in 1, 1/5 in 2 | 5.4k to 6.6k | 2.5 to 4 min |
| LFM3-1.7B Q8_0, llama.cpp b12058, 256k context | US | 2/5 in 2 of 7 runs, 1/5 in 1, invalid JSON in 4 | 11k to 23k | 5 to 12 min |
| Mistral Devstral Small 2 24B Q8_0, llama.cpp b12058, 64k context | France | 1/5 in 2 of 9 runs, 0/5 in 7 | 0.3k | 1 to 2 min |
| Gemma 4 26B-A4B Q8_0, llama.cpp b12058, 256k context | US | 0/5 in 4 of 4 runs | 3.0k to 5.3k | 3 to 5 min |
| Minico M1 1105 (350M) Q8_0, llama.cpp b12144, 32k context, `reasoning_effort:high` | LFM2.5-350M base (Liquid AI, US) | 0/5 in 4 of 4 runs, no findings | 0.6k to 0.7k | 2 to 3 s |
| GPT-6 Sol (hosted) | US | 5/5 in 14 of 14 runs | 1.4k to 2.0k | 13 to 21 s |
| Claude Opus 5.5 (hosted, default effort) | US | 5/5 in 4 of 4 runs | 2.0k to 2.1k | 17 to 18 s |
| Gemini 4 Argon (hosted, default effort) | US | 5/5 in 4 of 4 runs | 0.7k to 0.8k | 15 to 33 s |
| GPT-6.1 Sol (hosted, default effort) | US | 5/5 in 4 of 4 runs | 1.2k to 1.3k | 19 to 20 s |
| GPT-6.1 Terra (hosted, default effort) | US | 5/5 in 4 of 4 runs | 1.5k to 1.7k | 19 to 21 s |
| Claude Haiku 5.5 (hosted, default effort) | US | 5/5 in 3 of 4 runs, 4/5 in 1 | 2.1k to 2.8k | 8 to 12 s |
| GPT-6 Luna (hosted, default effort) | US | 5/5 in 2 of 4 runs, 4/5 in 2 | 1.6k to 2.2k | 14 to 21 s |
| Grok 4.7 (hosted, default effort) | US | 5/5 in 2 of 4 runs, 4/5 in 2 | 6.3k to 14k | 1.3 to 2.8 min |
| Claude Sonnet 5.5 (hosted, default effort) | US | 3/5 in 2 of 4 runs, 2/5 in 1, 1/5 in 1 | 1.0k to 1.1k | 8 to 9 s |
| Claude Opus 5.5 (hosted, effort high) | US | 5/5 in 4 of 4 runs | 1.8k to 2.3k | 16 to 20 s |
| Claude Haiku 5.5 (hosted, effort high) | US | 5/5 in 4 of 4 runs | 2.7k to 3.4k | 11 to 14 s |
| Grok 4.7 (hosted, effort high) | US | 5/5 in 4 of 4 runs | 5.7k to 11.9k | 1.2 to 2.7 min |
| GPT-6.1 Sol (hosted, effort high) | US | 5/5 in 3 of 4 runs, 4/5 in 1 | 2.0k to 2.6k | 35 to 44 s |
| GPT-6.1 Terra (hosted, effort high) | US | 5/5 in 3 of 4 runs, 4/5 in 1 | 2.2k to 3.1k | 27 to 39 s |
| Gemini 4 Argon (hosted, effort high) | US | 5/5 in 3 of 4 runs, 4/5 in 1 | 0.7k to 0.8k | 38 to 55 s |
| GPT-6 Luna (hosted, effort high) | US | 5/5 in 2 of 4 runs, 4/5 in 1, 1/5 in 1 | 2.2k to 6.0k | 30 to 76 s |
| Claude Sonnet 5.5 (hosted, effort high) | US | 5/5 in 2 of 4 runs, 3/5 in 1, 1/5 in 1 | 1.0k to 2.1k | 7 to 15 s |

GPT-6 Sol counts its hidden reasoning against the output limit: with the
old 2,048-token hosted default, two runs were cut off and lost most of their
findings, so the default is now 16,384 (you pay only for tokens used). Two
correct findings it worded or anchored differently were scored as misses
until the scorer was widened. No run reported a false positive. Bonsai missed the mutex left locked on a
cache hit at both efforts. Flint Q4_K_M generated about 50 tokens/s, the
fastest local model here, and Q8_0 about 37; neither found all five in any
run. Q4_K_M found the mutex bug in 2 of 8 runs and the negative refund in
3 of 8 (the context size made no clear difference), and in all four 256k
runs it also flagged the never-initialized cache map, which is only a bug if
callers use a zero `Store`. Q8_0 found the mutex bug in 2 of 4 runs and the
negative refund in 3 of 4 but missed the stale cache twice. Both flagged the
ignored refund-amount parse error, a real bug that was not planted. The Q8_0
runs left 5% of memory free with the Mac swapping, so its times
are pessimistic. Liquid's models trail the rest: LFM2.5-24B-A2B (about 69
tokens/s) never found the mutex or stale-cache bugs and missed the SQL
injection once, though it reported nothing false. LFM3-1.7B (about 85
tokens/s) returned invalid JSON in four of seven runs, even after the
repair retry, by not escaping quotes in code it cited as evidence, and two of
its three completed runs claimed the refund UPDATE subtracts the amount from
itself, the first false positives in these measurements.

The November 2026 batch (Gemma, Granite, Nemotron, North, Devstral) used
each vendor's recommended sampling, thinking switched on where the template
has a switch, and `--reasoning-param none`. Two lessons from it:

- **Line numbers decide the score.** The diff has no line numbers, so a
  model counts them from the hunk header, and overload drops any finding
  whose evidence is not on the cited line. Gemma 4 26B-A4B and Devstral
  Small 2 both found the SQL injection and the pagination bug in captured
  replies but cited lines 4 to 9 off (45 or 50 instead of 41), so every
  finding was dropped. Gemma 4 31B, Nemotron and North counted correctly.
  Devstral is also not a reasoning model and answered in about 290 tokens.
- **Dense 30B models at Q8 do not fit a 64 GB Mac in practice.** With an
  IDE, language servers and a VM using about 20 GB, Gemma 4 31B (33 GB) and
  Granite 4.2 30B (31 GB) were pushed into swap and generated about 3
  tokens/s; Granite's second run hit the 90-minute eval limit and the rest
  were stopped. Mixture-of-experts models (Flint, Nemotron, North) read far
  fewer weights per token and stayed usable. On this tier use Q4 for dense
  models, or a Mac with 96 GB or more.

Gemma 4 31B and North Mini Code matched each other at 12 of 20; Flint Q8
remains the best non-Chinese model measured (15 of 20). North was the
noisiest, adding findings such as an uninitialized cache map, GET used for a
state change and database errors returned to the client. Nemotron is from a
US company, but its model card lists training data generated with DeepSeek,
Qwen and GLM models; North (Cohere) is Canadian and Devstral (Mistral) is
French, so check each against your own policy.

TensorFold 1.2.2 (the macOS release archive, no install needed) served
Nemotron's MLX 4-bit checkpoint at about 40 to 64 tokens/s on the M1 Max,
roughly twice llama.cpp's Q8 speed, but found fewer bugs (6 of 30 against 8
of 20); its replies were valid and line numbers correct, so the 4-bit
weights, not the engine, are the likely cause. Its MTP drafter is broken on
this M1 with sampling on: with temperature 1.0 the reasoning collapsed into
one repeated token until the limit, and with temperature 0 it was correct
but slower than drafts off (29 against 48 tokens/s), so these runs used
`--no-drafts`. Gemma 4 31B could not be tested: TensorFold's Gemma path
needs M5 tensor units and refuses M1 to M4.

November 6 batch:

- **Ripple-65B Preview** (Ricursive Intelligence, Palo Alto; trained from
  scratch, Apache 2.0, a dense 65B with mostly sliding-window attention)
  is the first non-Chinese model to find all five bugs in a run, and did
  it at 3 bits (14 of 20, against Flint Q8's 15). It found the negative
  refund, the stale cache and the endless loop on a non-positive page size
  that most models miss, with no false positives. But a dense 28 GB model
  generates about 2.5 tokens/s on the M1 Max, 36 to 72 minutes per file, so
  it is not practical here. Support landed in llama.cpp on November 5
  (PR 31692); no release build had it yet, so these runs used a llama.cpp
  built from master and a community GGUF (AtomicChat, made with that PR).
- **TwIL-8B** (webAI, Austin; Granite 4.2 8B post-trained) found 11 of 20 at
  Q8, remarkable for 8B, but only 4 of 20 at Q4 "balanced", where one run
  put code in the line field and was rejected. Its webAI non-commercial
  licence rules it out for most business use without a separate licence.
- **Minico M1 1105** is a 350M LFM2.5 fine-tune; it answered in seconds with
  no findings. Too small for code review.
- **Meridian Air CivoE** (webAI) has no public weights: the Hugging Face
  repositories hold LoRA adapters for an internal Meridian-1 host model, so
  it cannot be run locally on any hardware.

Hosted baseline (November 6): every model went through the Cloudflare AI
Gateway's OpenAI-compatible endpoint with overload's hosted defaults (no
reasoning field sent, 16,384 output tokens), four runs each, all eight
models in parallel. Opus 5.5, Gemini 4 Argon, GPT-6.1 Sol and GPT-6.1 Terra
found all five bugs in every run, and Opus and Argon added nothing beyond
the planted bugs; Sol, Terra, Grok and Luna added 4 to 7 extra findings
across four runs (mostly the ignored parse error and the non-positive page
size, both real). Haiku 5.5 and Luna missed only the stale cache once or
twice; Grok missed the SQL injection twice and was the slowest by far.
Sonnet 5.5 scored lowest (9 of 20) because it cited lines one or two off
(42 for 41, 65 for 63), so its mutex and pagination findings were dropped
or fell outside the scorer's window, the same placement problem as Gemma 4
26B-A4B and Devstral locally. Reflection Beam 1.0 through Workers AI
(`workers-ai/reflection-ai/beam-1.0-501b-a23b`) could not be measured: the
gateway account has no unified billing credits for it.

The default-effort Claude runs above had **no thinking**: the gateway's
OpenAI-compatible endpoint ignores `reasoning_effort` for Anthropic and
refuses adaptive thinking ("not available via the OpenAI compatibility
endpoint"). The effort-high rerun therefore sent Claude models through a
small local proxy to the gateway's native Anthropic endpoint with
`output_config.effort: "high"` and `thinking: {"type": "adaptive"}`; the
OpenAI, xAI and Google models got top-level `reasoning_effort: "high"`
through the compatibility endpoint (reasoning tokens rose for OpenAI, and
Argon took about 2.5 times longer). At high effort Opus 5.5, Haiku 5.5 and
Grok 4.7 found 20 of 20; Sol, Terra and Argon 19; Luna 15 (one run found
only the SQL injection); Sonnet 5.5 14. Sonnet was bimodal: the two runs
where it used about 2k tokens (adaptive thinking engaged) found all five,
and the two at about 1k tokens repeated its line-number misses. With five
bugs the eval is near its ceiling for frontier models, so one-bug
differences between them are noise. Overload now calls Claude through the
native Messages API (`--provider anthropic`, see Anthropic models above),
so Claude reviews can think.

This is one
synthetic file; treat it as
directional. Parallel files did not help on the M1 Max (two streams gave
10 tokens/s combined against 13 for one); it is meant for GPUs and hosted
APIs, where eight at once cut a 21-file review from 138 s to 24 s.

Local connections have no response-header timeout, because a non-streaming
reply sends nothing until all thinking is done; the review timeout (260
minutes by default) still bounds every call.

### Bake-offs

A bake-off runs the same reviews through several saved models and compares
what each found, where it pointed, how long it took and how many tokens it
used, so a team can choose models on evidence from its own code. Start one
under Bake-offs, or:

```sh
overload bakeoff run --models flint,opus --suites planted --runs 3
overload bakeoff show 4
```

- The built-in `planted` suite holds the planted-bug file the measurements
  above use (the same five bugs) and a file with a misleading name that
  hides a SQL injection. Bake-offs score it more strictly than the
  measurements table: every bug needs a finding within two lines of its
  range that uses the expected words, while the table credits the mutex
  bug on its lines without any wording or on any line that mentions a
  deadlock or unlock, the stale cache bug on any line, and the pagination
  bug on line 63 without any wording. A model can therefore score lower in
  a bake-off than in the table. `TestPlantedBugEval` reviews the suite's
  file and logs both counts, so reruns stay comparable with the table.
- Build cases from your own history: pick commits that fixed a real bug.
  `overload bakeoff case --repo . --commit <fix>` prints a case in which
  the reviewer sees the change that undoes the fix (the buggy code as new
  lines) and is expected to point at the lines the fix changed. Save cases
  as a suite with `overload bakeoff suite save NAME case1.json case2.json`.
  Only modified text files are included, at most 20 per commit and 512 KiB
  in all; added, deleted, renamed and binary files are skipped, and merge
  commits are refused. Your git configuration (colours, external diff
  tools, path prefixes) does not change the case.
- All the lines a fix changed form one bug: a finding near any of them
  finds it, and it counts once. To score a fix as several bugs, give its
  expected findings different `bug` names in the case file.
- A bug counts as found when a finding cites its file within two lines of
  the expected range and, for planted bugs, uses the expected words (each
  must start a word, so "lock" does not match "block"). **Misplaced**
  counts bugs a model described in the right file but cited on the wrong
  lines, which overload drops before posting (the problem that sank
  Gemma 4 26B-A4B and Devstral above). It relies on the expected words, so
  it shows n/a for cases built from commits. **Other findings** matched no
  expected bug and leave out the misplaced ones; some may be real bugs
  nobody planted or fixed, others false alarms.
- Bugs found, time and tokens cover completed reviews. A review that
  failed, timed out (each review has the server's review timeout), was
  refused by the model policy or left files unreviewed counts only as
  failed, and a model whose reviews all failed shows n/a.
- Each model reviews with its connection's prompt. Reviews run one at a
  time on the server, on their own queue so pull request reviews are not
  held up, with the model policy checked before every review; a model the
  policy refuses is recorded as failed rather than called. A bake-off runs
  at most 300 reviews (models times cases times runs) and 10 models. A
  restart ends a running bake-off as failed, without recording the review
  it cut short; start it again.
- Bake-offs run reviews in the server process even when pull request
  reviews run in sandboxes: a case is a few files held in memory that the
  reviewer only reads, and no repository code or tool runs.

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

On a Mac, **GitHub → Set up Cloudflare Tunnel** in the UI does this in a few
clicks; [docs/cloudflare-tunnel.md](../docs/cloudflare-tunnel.md) covers both
that and a manual setup.

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

### The review as a merge gate

Overload sets an `overload` commit status on every reviewed head (with the
GitHub App's "Commit statuses: write" permission), and with a review
decision of request changes or approve it posts `REQUEST_CHANGES` or
`APPROVE` reviews. The status is built to be a required check:

- It fails when the review requested changes, and when any changed file was
  not reviewed: too large for the model's context, assigned to an agent
  that failed, or missing from the review's routing. The changed files are
  read from the pull request diff on the overload server, not taken from
  the sandbox's report. A review that never finishes leaves the status
  pending or sets an error; nothing that did not finish passes. Files on
  the workflow's skip paths are excluded on purpose. Deleted, binary and
  mode-only files, and the old names of renamed files, have no lines to
  review, so they do not fail the check, but the path rules below still
  apply to them.
- It is computed the same way when posting is off, so it can gate merges
  before comments are turned on; the description then says "Would approve"
  or "Would request changes" and "not posted".
- When the same commit is reviewed again, only the newest review posts and
  sets the status, so an older verdict that finishes later cannot replace
  it. A failing verdict is sticky: a re-review of the same commit that was
  requested through the review API (an API key or a workload token) can
  add to it but cannot clear it, approve, or withdraw overload's request
  for changes; the first such review says why. Only a re-review a person
  starts from the Review button or `overload review --queue --again` can
  clear it. An automated re-review waits until the earlier review of the
  commit has been posted; a person's does not, and the older post is then
  skipped. A post that still fails after its last retry (about an hour and
  a half) is marked `post_failed`, records a failing verdict and fails the
  check, so it never keeps re-reviews waiting.
- When the changed files of a review are unknown, the check fails and the
  review says so, even with no findings. Reviews finished before this
  version did not record their changed files, so any still waiting to be
  posted at upgrade time fail this way until the pull request is reviewed
  again.
- **Human review paths** (workflow setting `human_review_paths`, globs such
  as `auth/**`, `**/migrations/**`, `.github/**`): a pull request touching
  them, including deleting them or renaming files into or out of them, is
  never approved by overload, and the review lists them whenever the set
  of such files differs from overload's last posted review. They match
  without regard to case, and a bare name such as `auth` also covers every
  file in an `auth` folder at any depth.
- **Approve only these paths** (`approve_paths`, used only with
  `review_decision` approve): overload approves only when every changed
  file matches these low-risk globs, such as `docs/**`, including skipped,
  deleted and binary files and both names of a rename; other pull requests
  get comments. Matching here is case sensitive. Overload never approves a
  review in which no file was reviewed, such as one whose files were all
  skipped.
- When a later review does not approve (it requests changes, comments, or
  is partial), overload dismisses its own earlier approval, so a stale
  approval never keeps counting. If GitHub refuses the dismissal, the
  check fails ("Could not withdraw overload's earlier approval") and the
  dismissal is retried.
- Pull request text stays untrusted: models only produce findings, and the
  decision comes from finding severities and the rules above.

Recipe for a protected branch (Settings, Branches or Rules):

1. Require status checks to pass, select `overload`, and set its expected
   source to the overload GitHub App ("Any source" lets anyone who can
   write commit statuses, such as a CI token with `statuses: write`, pass
   the check). Require branches to be up to date, so the check is for the
   head being merged.
2. Require a pull request review, and turn on "Dismiss stale pull request
   approvals when new commits are pushed", so no approval, overload's or a
   person's, carries over to code it did not see. If "Restrict who can
   dismiss pull request reviews" is on, add the overload GitHub App to the
   allowed list, or overload cannot withdraw its own approval and the
   check stays failing.
3. Add a `CODEOWNERS` file naming people for the same sensitive paths as
   `human_review_paths`, with "Require review from Code Owners" on.
   Overload never approves those pull requests, so a person always has to.
4. Decide whether overload's approval may count. Check in a test repository
   whether your plan counts a GitHub App's approval toward the required
   reviews; if it does and you want bot-only merges, limit them with
   `approve_paths`, otherwise keep `review_decision` at request changes.

## Signing in

A single-user install signs in with the generated Basic Auth login. Teams
can sign in with an OpenID Connect provider whose ID tokens list the
person's groups (Okta, Keycloak, Authentik, Dex, or Entra ID with the notes
below) instead; Cloudflare Access remains an alternative front door, but
not together with OIDC.

| Variable | Meaning |
|---|---|
| `OVERLOAD_OIDC_ISSUER` | The provider's issuer URL; turns OIDC sign-in on |
| `OVERLOAD_OIDC_CLIENT_ID`, `OVERLOAD_OIDC_CLIENT_SECRET` | The client registered for overload |
| `OVERLOAD_BASE_URL` | The https address people use (http only for localhost) |
| `OVERLOAD_OIDC_ADMIN_GROUPS`, `OVERLOAD_OIDC_MEMBER_GROUPS`, `OVERLOAD_OIDC_VIEWER_GROUPS` | Comma-separated provider groups for each role |
| `OVERLOAD_OIDC_GROUPS_CLAIM` | The ID token claim that lists groups (default `groups`); a dotted name such as `realm_access.roles` reads a nested claim |
| `OVERLOAD_OIDC_SCOPES` | Extra scopes, if the provider needs one to include groups |
| `OVERLOAD_SESSION_HOURS`, `OVERLOAD_SESSION_IDLE_HOURS` | Session length (default 12) and idle limit (default 2), in hours up to 720 |

Register `<base>/auth/callback` as the client's redirect URI and
`<base>/auth/signed-out` as its post-logout redirect URI.

- **Entra ID** puts group object IDs (GUIDs) in the `groups` claim, so map
  the GUIDs, or configure the app to emit group names (cloud-only groups)
  or app roles with `OVERLOAD_OIDC_GROUPS_CLAIM=roles`. Someone in more
  groups than Entra puts in a token (the overage limit, about 200) gets no
  `groups` claim and is refused; assign the app to groups or use app roles.
- **Google Workspace** ID tokens carry no groups, so it cannot assign roles
  on its own; put a provider that adds groups in front of it (Dex,
  Keycloak or Authentik brokering Google sign-in).

How it works:

- Sign-in uses the authorization code flow with PKCE, and checks the
  `state` (bound to the browser by a cookie) and `nonce`. ID tokens are
  verified against the provider's discovery document and keys. A sign-in
  started at another address (such as `127.0.0.1`) is sent to the base URL
  first, since its cookie must reach the callback. At most 10,000
  sign-ins can be in progress; past that the oldest are dropped, so
  anonymous requests cannot fill the database.
- The provider's discovery document is fetched once, when someone first
  signs in; if the provider is down, sign-ins fail at once for 30 seconds
  before it is tried again, and overload keeps serving signed-in people.
- Sessions live in Postgres; the browser holds an opaque HttpOnly,
  SameSite=Lax cookie (`__Host-overload_session`, Secure, over https), and
  only its hash is stored. Signing in again ends the browser's previous
  session. Signing out, or an administrator disabling the person under
  Users (or `overload users disable ID`), ends sessions at once. Each
  session has its own form token, so forms rendered for one person do not
  work for another.
- Roles come from the groups at every sign-in. Admins change anything;
  members also queue reviews and run schedules; viewers only read. People
  in no mapped group are refused.
- Signed-out page loads go to sign-in; scripts, stylesheets and event
  streams do not start sign-ins.
- Break-glass: with `OVERLOAD_UI_USER` and `OVERLOAD_UI_PASSWORD` also set,
  open `/?break-glass=1` to sign in with that shared login as an admin
  while the provider is down (the sidebar says so and links Users);
  changes made that way are logged with the client address and browser.
- Signing out ends the overload session; the signed-out page links to the
  provider's own sign-out, with the session's ID token as `id_token_hint`
  (Okta requires it), when the provider advertises one.

Tests use an in-process OIDC provider. SCIM provisioning and a test
against Dex in kind are later additions.

## Model gateway and API keys

Overload serves its saved models to agents and CLIs (Claude Code, OpenCode,
Cline, Crush) through OpenAI and Anthropic compatible endpoints on the same
address as the UI:

| Endpoint | For |
|---|---|
| `GET /v1/models` | The saved models the model policy allows |
| `POST /v1/chat/completions` | OpenAI-compatible model connections |
| `POST /v1/messages` | Anthropic model connections |

Clients authenticate with an overload API key (`Authorization: Bearer
ovl_...` or `x-api-key: ovl_...`) and name a model by its name on the Models
page. Overload swaps in the connection's model ID and API key, so provider
keys never leave the server. OpenAI clients use `<base>/v1` as their base
URL, Anthropic clients `<base>`.

```sh
export OPENAI_BASE_URL=https://overload.example.com/v1 OPENAI_API_KEY=ovl_...
export ANTHROPIC_BASE_URL=https://overload.example.com ANTHROPIC_API_KEY=ovl_...
```

- Keys are created on the API keys page (members and admins; each person
  sees their own, admins see all, up to the 500 newest) or with
  `overload keys create --name ci` for shared keys. Their one scope today
  is `models`; they expire after 1 to 365 days, are shown once and stored
  only as a hash. Revoking asks for confirmation first.
- Only members and admins can use models: a viewer's key is refused, and a
  person whose role drops below member at sign-in has their keys revoked.
  Disabling someone under Users revokes their keys at once. Removing
  someone in the provider does not: their keys keep working until they
  next sign in or are disabled in overload (there is no SCIM provisioning
  yet), so disable people in overload when they leave.
- Requests for one model share its Parallel files setting as slots. When
  they are busy, waiting requests are served in turn per person, so one
  agent sending many requests cannot starve the others; at most 64 wait per
  model and 16 per person, then the gateway answers 429. Calls are limited
  to 90 minutes for local models and 15 for hosted ones.
- Every forwarded call is recorded in tokens and minutes (streamed
  responses included: overload always asks OpenAI-compatible servers for
  stream usage and drops that chunk again for clients that did not ask for
  it). Anthropic's cached input (cache reads and writes) counts as input,
  as OpenAI's cached prompt tokens already do. Requests refused before
  reaching a model (bad key, unknown model, full queue) are not recorded.
  The Usage page shows the last 30 days per person and model.
- Callers' own credentials and `anthropic-beta` headers are never passed
  on, and redirects from the model server are not followed. When the model
  server refuses overload's credentials the gateway answers 502, so a bad
  model API key is not mistaken for a bad overload key.
- The gateway does not translate between the two APIs; call each model
  with its own. OpenAI-compatible models are served through Chat
  Completions only, including models saved with the Responses API, so
  those must also accept `/chat/completions`. Reviews and scheduled jobs do
  not go through the gateway's queue, so a model shared by both may see
  more requests than its slots.
- `/v1/*` is authenticated by API keys alone: it skips the UI's sign-in,
  Cloudflare Access and host check, so in-cluster agents can call
  `http://overload.overload.svc:8082/v1` (the cloudflare overlay's network
  policy admits only cloudflared, so allow them there too). When Cloudflare
  Access guards the hostname, add a bypass policy for `/v1/*` at the edge,
  or agents cannot reach the gateway.

## Workload identity

Automation can request reviews and call models with its own short-lived
OIDC token instead of a stored secret. Under Workloads, an admin adds rules
naming the token issuer, the audience, claim conditions (all must match;
`*` matches within one path segment) and scopes (`reviews`, `models`).
Tokens are verified against the issuer's published keys, and only issuers
some enabled rule names are ever contacted.

GitHub Actions (issuer `https://token.actions.githubusercontent.com`): a
rule must match `repository` or `repository_owner`; add `ref`,
`environment` or `job_workflow_ref` to narrow it further.

```yaml
name: overload review
on:
  pull_request_target:
    types: [opened, synchronize, reopened]
jobs:
  review:
    runs-on: ubuntu-latest
    permissions:
      id-token: write
    steps:
      - env:
          REPOSITORY: ${{ github.repository }}
          PULL_REQUEST: ${{ github.event.pull_request.number }}
        run: |
          token=$(curl -sfH "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
            "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://overload.example.com" | jq -r .value)
          curl -sf -X POST https://overload.example.com/api/reviews \
            -H "Authorization: Bearer $token" \
            -d "{\"repository\":\"$REPOSITORY\",\"pull_request\":$PULL_REQUEST}"
```

A `pull_request_target` job runs with the base repository's permissions
and secrets, so it must never check out or run the pull request's code:
no `actions/checkout` of the head, no build or test steps. It only asks
overload for a review, and overload reads the pull request itself.

`POST /api/reviews` with `{"repository": "owner/name", "pull_request": N}`
queues a review of the pull request's current head, as the Review button
does, and answers 202 with the run ID. It accepts a workload token with
the `reviews` scope or an API key with that scope. A token that names a
repository (every GitHub Actions token does) can only request reviews of
that repository. Requesting a review never approves anything by itself;
the review's own result decides, and a review requested this way cannot
clear an earlier failing verdict on the same commit (see the merge gate
above). Workload tokens with the `models` scope can also call the model
gateway.

Do not give the `reviews` scope to `pull_request` jobs. They run the
workflow file from the pull request itself, so its author can change what
the job sends, for example asking for review after review of the same
commit with `"again": true`. Use a `pull_request_target` workflow, which
runs the default branch's file, and narrow the rule's `job_workflow_ref`
to that file on the default branch (such as
`owner/name/.github/workflows/review.yml@refs/heads/main`), or require an
`environment` with required reviewers.

Kubernetes service accounts work the same way with the cluster's
service-account issuer as the rule's issuer (projected tokens with an
audience of overload's address); overload must be able to read that
issuer's discovery document, which on most clusters needs the
`system:service-account-issuer-discovery` role bound to unauthenticated
users or a public issuer URL. Match `sub`
(`system:serviceaccount:<namespace>:<name>`).

## Linux servers

`overload install` on Linux writes the configuration and database (with
`--postgres-bin` or `--database-url`) and prints the `overload serve` command;
it does not manage services. Run that under systemd or a container. The app
image is built from `deploy/images/app/Dockerfile` and published to
`ghcr.io/daltoniam/overload` on release tags.

## Kubernetes

Tested on disposable kind clusters (kind v0.33, Agent Sandbox v1.0.4) and,
with `overlays/cloudflare` and `overlays/cloudflare-sandbox` (Agent
Sandbox reviews with hosted models through the model proxy), on
DigitalOcean Kubernetes.

- `deploy/k8s/base`: namespace, non-root app Deployment (read-only root,
  `/tmp` emptyDir, probes on `/healthz`), Service and ServiceAccount. It
  expects a Secret named `overload` with `DATABASE_URL`, `OVERLOAD_UI_USER`
  and `OVERLOAD_UI_PASSWORD`; bring your own Postgres.
- `deploy/k8s/components/postgres`: a single-replica Postgres StatefulSet
  and a network policy that lets only the app reach it.
- `deploy/k8s/overlays/kind`: base plus `components/postgres`.
- `deploy/k8s/overlays/cloudflare`: base, `components/postgres` and
  cloudflared; only cloudflared can reach the app. Cloudflare Access guards
  the UI (`OVERLOAD_ACCESS_TEAM_DOMAIN`, `OVERLOAD_ACCESS_AUD`). See
  [docs/cloudflare-kubernetes.md](../docs/cloudflare-kubernetes.md).
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
- [x] GitHub App and repository sync against a real App and repository.
- [x] Posting to a real pull request.
- [ ] A published release and Homebrew formula, installed on a clean Mac.
- [ ] A reaper for sandbox claims left by a crashed worker.
- [ ] Backup, restore and upgrade tests.
