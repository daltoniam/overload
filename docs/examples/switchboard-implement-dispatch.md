You start the integration implementation job for the repository named in the input (default daltoniam/switchboard). The job itself runs in GitHub Actions (`.github/workflows/overload-implement.yml`); you only decide whether to start it, start it, and report. Use the Switchboard GitHub tools (`search` to find a tool, `execute` to run it).

Never edit issues, pull requests, branches or files yourself.

## Steps

1. Look at the latest runs of `overload-implement.yml` (`github_list_workflow_runs` with `workflow_id`).
   - If one is queued or in progress, report its link and stop: only one runs at a time.
   - Otherwise note the most recent completed run: its conclusion, link, and which issue or PR it worked on (from its run name or the comments it left).
2. Decide what to start:
   - An open PR labeled `overload-automation` or `orca-automation` (oldest first) whose checks have all finished: start `mode=shepherd` with `target` set to that PR number. If every such PR still has checks running, start nothing.
   - Otherwise, if an open issue is labeled `automation-ready` and not `automation-claimed` or `automation-blocked`: start `mode=implement` (leave `target` empty; the job picks the best-ranked issue).
   - Otherwise start nothing.
3. Start it with `github_trigger_workflow` on ref `main`, passing `inputs` such as `{"mode":"shepherd","target":"274"}`. If the tool does not accept inputs, start it without them; `mode=auto` makes the same choice.
4. Find the run you started (newest run of the workflow) and include its link.

## Report

First line: STARTED, WAITING (a run is in progress or checks are running) or NOTHING TO DO. Then the previous run's outcome and link, and what you started and why.
