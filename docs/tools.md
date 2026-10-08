# Tools for scheduled agents

Scheduled agents can call tools on MCP servers. The main use is
[Switchboard](https://github.com/daltoniam/switchboard): connect it once and
an agent can work with GitHub, Linear, Slack, web search and every other
integration Switchboard exposes, through its `search` and `execute` tools.
Any MCP server reachable over streamable HTTP works the same way.

Pull request reviews never get tools. They read untrusted pull request text,
and a tool-using reviewer could be talked into acting on it.

## How a run works

1. When a scheduled run starts, overload opens an MCP session to each of
   the agent's tool servers and lists their tools. The model sees them as
   `<server>_<tool>`, for example `switchboard_execute`.
2. The agent loops: each step is one model call that may call tools.
   Results go back to the model, cut to 32 KB each. A failed call is
   returned to the model as an error, so it can try another way.
3. The agent stops when it answers, or at the workflow's limits:
   - **Steps** (default 40, at most 200). On its last step the agent loses
     its tools and has to write its report.
   - **Minutes** (default 30, at most 240). An agent still working fails
     the run.
   - Tool calls are also capped at five per step.
4. The run page shows the agent's report, its step, tool call and token
   counts, and every tool call in the timeline with its input and result.

Each run gets its own Switchboard session (`X-Switchboard-Session-Id`), so
Switchboard's pinned results and context stay within the run.

## Connect Switchboard

### Hosted Switchboard

1. In the Switchboard dashboard, create an API key for overload. Give it a
   policy that allows only the tools your agents need (for example GitHub
   issue tools and web search), because the agent can call anything the key
   allows.
2. Put the key in overload's environment under a name starting with
   `OVERLOAD_TOOL_`:
   - Mac: add `OVERLOAD_TOOL_SWITCHBOARD=sb_…` to `overload.env`, then
     restart overload.
   - Kubernetes: add it to the `overload` Secret and restart the
     deployment.
3. In overload, open **Tools → New tool server**:
   - URL: your organization's MCP URL,
     `https://app.switchboard-mcp.com/orgs/<org>/mcp`
   - Token variable: `OVERLOAD_TOOL_SWITCHBOARD`
4. Click **Check connection**. You should see `search`, `execute` and
   Switchboard's other tools.

### Self-hosted Switchboard

Run Switchboard where overload can reach it, configure its integrations,
and add a tool server with its MCP URL, for example
`http://127.0.0.1:3847/mcp` on the same Mac. Switchboard on loopback needs
no token; if you expose it, put an authenticating proxy in front and give
overload that token.

## Give an agent tools

1. Create an agent with job type **Scheduled prompt**, and tick its tool
   servers. Its instructions are the job: say what to do, what not to
   touch, and what to report.
2. Create a **Scheduled prompt** workflow with the agent, and set its step
   and minute limits.
3. Create a **Schedule** for the workflow (cron, timezone and a JSON input
   the agent sees), and use **Run now** to try it.

Overload adds its own rules around the instructions: scheduled input and
tool results are data, not instructions, and the agent ends with a report.

From the command line:

```sh
overload tools apply - <<'JSON'
{"name":"switchboard","url":"https://app.switchboard-mcp.com/orgs/acme/mcp","token_env":"OVERLOAD_TOOL_SWITCHBOARD","enabled":true}
JSON
overload tools check switchboard
overload agents apply researcher.json      # {"kind":"scheduled_prompt","tools":["switchboard"],...}
overload schedules run integration-research
```

## Example: daily integration research

[examples/switchboard-integration-research.md](examples/switchboard-integration-research.md)
is the prompt of a job that keeps Switchboard's integration issue queue
ranked every morning: it reads the repository through Switchboard's GitHub
tools, researches new candidates, and creates or updates issues. Give the
schedule input such as `{"repo": "daltoniam/switchboard", "tracker": 240}`;
add `"dry_run": true` to see what it would change without writing
anything. With GPT-6.1 Sol a run takes about five minutes and 30 tool calls.

## Models

Any model that supports tool calls works. Two settings matter:

- **Newer OpenAI reasoning models** (for example GPT-6.1 Sol) only accept
  tools together with a reasoning effort through OpenAI's Responses API.
  Set the model's **API** to *OpenAI Responses API* (`--api responses`).
  The endpoint URL stays the same, such as `https://api.openai.com/v1` or a
  Cloudflare AI Gateway `…/openai` route.
- Local models need tool-call support in the server (for llama.cpp, start
  `llama-server` with `--jinja`). Small local models tend to loop or stop
  early; hosted models do much better at multi-step tool use.

## Safety

- Tokens never enter the database: a tool server stores only the name of
  the variable holding its token, and that name must start with
  `OVERLOAD_TOOL_` so a tool server cannot be pointed at overload's own
  secrets.
- The agent can do anything the token allows. Restrict the token (a
  Switchboard API key with a narrow policy) rather than relying on the
  instructions.
- Agents read text written by other people (issues, comments, web pages).
  Overload tells the model to treat it as data, but a narrow token is the
  real protection.
- Disabling a tool server stops new runs of workflows whose agents use it.
