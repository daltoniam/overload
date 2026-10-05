# Overload UI improvement plan

**Status: UI implementation delivered 2026-10-04. Go/templ retained; HTMX 2.0.11 is vendored locally.**

Implemented shared spacing/theme controls, per-page server search/filter fragments, database-backed run/webhook pagination, model-create dialog and advanced settings, deletion confirmation with native fallback, retained validation input, workflow-dependent agent options/ordering, pinned revision display, repository detail redirects, structured event refresh/polling, multiline results, and truthful Overview/GitHub states.

Verification: full Go tests, vet, lint, build, and whitespace checks passed. An isolated Postgres database verified search beyond the former 100-row limit and literal wildcard handling. Chrome checks passed for light/dark, route rendering, model dialog, workflow type selection, delete confirmation, Escape, mobile overflow, and console/CSP errors. Existing-data Overview/Models/Runs/Webhooks/GitHub returned 200. Browser snapshots are in [implemented screenshots](docs/ui-improve/implemented/).

Local UI is available at `http://127.0.0.1:8082` through an owned preview process reading the existing database without starting workers, migrations, scheduler, or model/GitHub calls. Fixture screenshots use isolated in-memory data; `live-*` screenshots use existing records. Remaining polish relative to the original aspirational handoff: per-field inline errors (current errors use a retained form plus summary) and opt-in animated sidebar navigation. No-JS workflow type links start a fresh matching form rather than retaining an unsaved draft. Native sidebar links remain intentionally unboosted. No unsupported execution controls were added.

Prepared 2026-10-04. The former Errand app now lives in `/Users/dalton/code/overload`; `/Users/dalton/code/errand` contains no application source. Implement this plan in Overload, not in the empty Errand directory.

## 1. Scope and approval boundary

Keep the simple sidebar, page heading, lists, and focused forms. Keep Go, templ, custom CSS, Postgres/River, and progressively enhance with HTMX. Improve model configuration, modal dialogs, search, list readability, spacing, feedback, run timelines/logs, and responsive behavior. Make light and dark equally polished with a System preference.

The existing application already implements Auto/Light/Dark using `localStorage['overload-theme']` and `data-theme`. Improve that implementation instead of building a second theme system. HTMX is **not currently included** in the inspected source, despite the requested target stack.

Approval authorizes the implementation sequence below, not new execution capabilities. Do not add retry/cancel/run-now, model health probes, webhook replay, graphs, global command search, or credential management features. Do not change review posting policy, model behavior, configuration identities, prompt revision semantics, or queued run snapshots.

The approved [style/brand guide](docs/ui-style-guide.md) is now wired into `AGENTS.md` for future UI work. This document remains the implementation handoff; the guide governs reusable visual and interaction rules.

## 2. Local stack and evidence

- Reused the already-running `./dist/overload serve` process at `http://127.0.0.1:8082`; no restart or second application instance.
- `/healthz` returned `ok`; every captured HTML route returned HTTP 200.
- Existing Docker database: `errand-postgres-1`, healthy, bound on loopback port 55433. Preserve its data and legacy name.
- `deploy/.env.local` and the native installer environment were absent. Do not run `make local-up` blindly against this live stack; its default configuration would create a different deployment.
- Used an isolated headless Chrome through an already-installed Playwright package because Chrome DevTools MCP reported a shared-profile conflict. Did not terminate anyone else's browser.
- Captured 33 full-page PNGs, covering all reachable page families, each create form, representative edit forms, both available prompt kinds, three run-detail states, five dark variants, and two mobile variants. This is route/template coverage, not a screenshot of every record in the database.
- Desktop captures: 1440 × 1000 viewport. Mobile: 390 × 844. Full-page screenshots can be taller. Manifest: [capture-manifest.json](docs/ui-improve/capture-manifest.json).
- No forms submitted, schedules enabled, GitHub apps created, live model calls made, or records changed during the audit.
- The checkout changed concurrently after capture: final verification found unrelated source edits, new guard/migration files (including final-prompt removal), and deleted local deployment scripts. This plan records the inspected/live UI snapshot, not those unreviewed changes. Before implementation, re-read current routes, enums, migrations, guards, and launch instructions; reconcile drift without reverting anyone else's work. Targeted HTTP/web tests passed; the full-checkout whitespace check reported an unrelated trailing blank line in `http/configuration.go`.

### Stitch provenance and limitation

Private Stitch project: `projects/8573415082752145101`, titled **Overload UI refinement: simple Go templ and HTMX**. Its generated reference asset is `assets/7569d6024edd445baee62fa893495862`.

**The available Stitch MCP schemas do not accept image attachments or provide a screenshot-upload/import tool. Existing screenshots were captured and inspected, but could not be fed to Stitch as binary images.** Stitch received detailed visual transcriptions of the live UI, route inventory, palette, layout, and backend constraints. This is text-guided generation, not image-conditioned redesign. Literal screenshot ingestion remains blocked by that API capability; an image-upload-capable Stitch interface is the required substitute if that is a prerequisite for approval.

Downloaded 13 initial concepts and 13 refined screenshots. Refinement removed most invented topbars, telemetry, unsupported actions, incorrect prompt kinds, and incorrect security copy. Use the **refined** screenshots below. Initial files without `-refined` are retained as provenance, not implementation targets. Stitch exports are design references, not source code or an approved brand guide.

### Precedence when a screenshot disagrees with the app

1. Approved requirements and actual backend validation/permissions.
2. This plan's explicit behavior and visual tokens.
3. Refined screenshots for composition and hierarchy.
4. Stitch-generated text, theme metadata, and export code, never authoritative.

Remaining mockup inaccuracies must **not** become features: fabricated workflow/agent IDs or runner host, repository “Active Sync”/installation ID, webhook listener URL, token estimates/Updated timestamps absent from view data, “final synthesis” execution claims, and “sequential pipelines” wording. Counts and sample records are illustrative. Do not copy invented Configure gates links. Use actual enums from source, not mockup display labels. Preserve supported bindings even if the mockup shows only a subset. Use System fonts rather than adding Geist/JetBrains/CDN dependencies. Do not scale the tiny downloaded Stitch preview's text into the production UI; use the sizes below.

## 3. Audit findings and proposed behavior

| Area | Current limitation | Proposed outcome |
| --- | --- | --- |
| Shell/spacing | Ad hoc gaps, oversized header, uneven overview columns | Retain 238px sidebar; consistent gutters, balanced directory cards, restrained headings |
| Theme | Existing cycling button; fixed dark code blocks | Explicit System/Light/Dark selector, complete semantic tokens, consistent forms/logs/dialogs |
| Lists | No search, filters, result feedback; Runs loads a capped list | Per-page search and native filters, truthful counts, database-backed run/webhook pagination |
| Models | Dense identifiers; long undifferentiated form | Secondary sanitized endpoint, type/default badges, essentials first, advanced disclosure; optional create dialog |
| Forms | Most errors are plain HTTP text; successful redirects have no notice | Keep entered values, error summary/inline messages, success notice, clear Cancel |
| Deletion | Immediate red submit with no confirmation | Accessible confirmation, dependency explanation, safe fallback page, unchanged server enforcement |
| Runs | Neutral badges for every state; little source context | Labeled semantic badges; readable source/trigger; no unavailable telemetry or actions |
| Run detail | Failure notice teal; zero findings can imply success; plain timeline paragraphs | Red failure treatment, state-specific results, timestamp/step/message event viewer, search/wrap/refresh |
| Prompts | Small textarea; pinned revisions not clear enough | Plain monospace editor, revision notice, immutable identities/types; no template execution |
| Workflow editor | Type changes do not refresh compatible agent options | HTMX-dependent select fragment, ordered accessible slots, max four distinct agents |
| Repositories | Binding actions stacked; some saves return to list | Compact binding table, side-by-side actions, preserve detail context and posting gates |
| GitHub | External manifest flow requires its own CSP; environment metadata may show ID 0 | Native full-document setup; truthful stored-credential copy; suppress unavailable metadata |
| Mobile/accessibility | Navigation initially expanded; wide tables; no dialog system | Compact native navigation, intentional table overflow, touch targets, keyboard/focus/error behavior |

“Logs” in this phase means **persisted run timeline events** (`At`, `Level`, `Step`, `Message` in the current source). Display the actual level when useful, but do not infer missing severity. There is no raw worker stdout stream or full production log browser in the current reader contract. Preserve multiline scheduled output and finding text. Migration 015 removes final prompts; current create/edit kind handling must remain entry/review, not mockup final synthesis.

## 4. Proposed visual foundation

This is a refinement of the existing teal identity, not a rebrand.

| Token | Light | Dark |
| --- | --- | --- |
| Canvas | `#f5f7f8` | `#111c26` |
| Surface | `#ffffff` | `#1c2a35` |
| Primary text | `#172635` | `#e6edf2` |
| Muted text | `#627282` | `#b1c1cd` |
| Border | `#dce4e9` | `#384958` |
| Accent | `#176b66` | `#78d5c0` |
| Accent soft | `#e6f3ef` | `#213d3d` |
| Danger text | `#b42334` | `#f18491` |
| Danger surface, proposed | `#fff1f3` | `#3b252e` |

Add paired tokens for success, warning, info, focus, backdrop, code, field, hover, disabled, and selection. Measure contrast rather than assuming Stitch's claims: text at least 4.5:1 (large text 3:1), meaningful control boundaries/focus at least 3:1. Danger button foreground is a dedicated token, not `--canvas` reused blindly.

- System UI stack, 14–15px body with ~1.5 line-height; 28–32px page title; 18–20px section heading; 12–13px secondary metadata.
- `ui-monospace` for model IDs, endpoints, SHAs, cron, JSON, prompt editor, timestamps. Do not use monospace for all navigation/labels.
- Spacing scale: 4, 8, 12, 16, 24, 32, 48px. Desktop main gutter 32–48px, panel padding 20–24px; mobile gutter 16px.
- Input/button visual height ~40px, touch targets at least 44px on mobile. Table rows minimum 48px, ~56px for secondary lines; allow wrapping to grow rather than clipping at fixed height.
- 8px control radius; 12px panel/dialog radius; restrained badges. Keep borders primary and shadows subtle. No gradient, glass, decorative charts, or animated status throbbing.
- Keep existing max-content constraint near 1160px; don't stretch forms across ultrawide monitors. Form content ~720px, model dialog ~640px with viewport-constrained scrolling.
- Keep all ten destinations and original order. Optional grouping labels must not change routes or bury navigation. Keep active section on child pages.
- At existing ~780px mobile breakpoint collapse sidebar into a compact header with a native Navigation disclosure. Stack toolbars/forms. Avoid icon-only collapsed navigation that needs a tooltip library.

## 5. Page-by-page target specification

### Overview (`/`)

Keep six directory cards; change the uneven grid into equal columns. Use a direct Overview title. Replace recent count-only card with latest five actual run rows using the shared compact table. Do not treat `len(ListRuns)` as the database's total. Replace blanket “no comments posted” copy with truthful opt-in explanation: posting requires repository flag, global server gate, and GitHub App. Do not introduce a gate-settings route.

### Models (`/settings`, `/settings/new`, `/settings/{name}`)

Search name/model ID, filter Local/Hosted, show a truthful configured/result count. Keep table, clickable model name, readable type, monospace ID, sanitized endpoint secondary line, parallel files, and Default for legacy reviews. Display no inferred health. Never expose URL userinfo or credential query parameters.

Split form into Connection essentials and Advanced review settings. Keep all current reasoning controls/effort values, max-token/concurrency bounds, API key environment **name**, immutable existing identity, and backend defaults (0 max tokens, minimum parallel 1). Preserve unknown/legacy values safely; do not replace semantics from screenshot sample values.

Enhance New model with a native `<dialog>` while `/settings/new` remains a real fallback page. Existing edit pages remain full pages initially. Inline validation must retain fields and never submit twice. No endpoint test probes. Cancel returns to list and preserves list query state; destructive action is separated and confirmed.

### Prompts, Agents, Workflows

All list pages share search/filters/counts and explicit resource-name links. Keep each editor on its dedicated route, not the mockup's simultaneous list-plus-editor composition.

Prompts: filter existing kinds, raw multiline textarea, existing name/type readonly plus submitted hidden fields, revision number and precise “saving creates a new revision; existing agents stay pinned” copy. Create kinds remain entry/review. Existing final records, if any, do not imply executable final support. No interpolation engine, notes field, token estimator, or new revision browser in this phase.

Agents: filters kind/enabled/model, show actual pinned prompt revision (not merely latest selectable revision), clear model/prompt prerequisite links. Entry selector only entry; review selector only compatible review prompts. Do not offer final execution. Preserve existing backend rules for job kinds and immutable identities.

Workflows: searchable list with kind/status, agent count. Editor type selector updates compatible agent options without discarding unrelated unsaved values. Keep saved disabled selections visible with explanatory text. Provide one to four distinct ordered selections and accessible up/down controls, no drag graph; order is configuration, not a guarantee of sequential execution. Server validates compatibility, distinctness, count, enabled rules.

### Repositories and bindings

Search owner/repo, filter enabled. Detail page keeps editable repository settings, compact PR binding table and inline add/update form, with actions on one row and visible status. Keep actual action values from backend. Add/toggle/delete binding stays on repository detail. Posting remains unchecked for a new repository; expose opt-in and gate prerequisites without pretending the checkbox alone means posting is active. Do not enable posting during visual tests. Keep removal restrictions and explain disabling to preserve history.

### Schedules

Search name/workflow, filter enabled. Separate cron and IANA timezone for scanning. Dedicated edit page has workflow, five-field cron, native IANA timezone input, plain JSON-object editor, enabled checkbox, Save/Cancel. Preserve real `NextRunAt` when supplied by store; do not fabricate it, and distinguish disabled schedules. No run-now or external delivery channel feature.

### Runs, run detail, timeline

List search ID/PR/trigger using fields that actually exist; filters actual kind/status enums; newest-first database pagination, stable ordering including ID. Render repository context only where available in model/store, not parsed from mockup strings. Keep native links/downloads.

Detail preserves error and existing/partial results honestly. For queued/running say “Results not available yet”; for failed explain results may be incomplete; for completed zero findings retain caveat that absence of findings is not proof of correctness. Never hide persisted partial findings solely because a run failed. Scheduled output and finding body preserve line breaks with escaped text (`white-space: pre-wrap`), not trusted HTML/Markdown execution.

Timeline becomes Timestamp / Step / Message with native wrapping toggle, search/filter count, and Refresh. Use existing event data; do not invent severity, worker names, or exit codes. Poll active runs at a modest ~3–5s cadence; stop terminal-state polling, pause hidden-tab updates, allow pause/resume, preserve scroll/selection. Do not animate every polled row or force auto-scroll while the user reads. Surface polling errors without clearing results. Raw stdout ingestion is explicitly deferred.

### Webhooks and GitHub setup

Webhooks searches repository/event/action, filters real outcomes, wraps long reason text, shows truthful page count and pagination. Retain existing installation-event records as appropriate; do not imply only PRs can be received. Rejected signatures are not persisted deliveries. No replay/resend/payload inspector.

GitHub setup keeps native full-document manifest continuation and callback navigation. Fields remain App name, optional Organization, **Public webhook URL**, HTTPS requirement and tunnel help. State truthfully that manifest credentials are stored in Postgres and never displayed. Configured/environment cases show only available app metadata, no misleading ID 0. Preserve installations list/status and disabled-by-default repository onboarding. No Save draft, avatars, or invented scope panels unless derived from actual manifest.

## 6. HTMX, CSP, and response contracts

Relevant entry points:

- `web/templates/layouts/base.templ`: shared CSS, shell, two theme scripts.
- `web/templates/pages/models.templ`: Models list/form.
- `web/templates/pages/pages.templ`: Overview, Runs, RunDetail, Webhooks.
- `web/templates/pages/automation_crud.templ`: prompts/workflows/schedules.
- `web/templates/pages/configuration_crud.templ`: agents/repositories/bindings.
- `web/templates/pages/github.templ`: setup, installations, external manifest form.
- `http/server.go`, `http/models.go`, `http/automation_crud.go`, `http/configuration.go`, `http/github_setup.go`: routes/validation.
- `http/theme.go`: CSP hashes extracted from exactly two inline theme scripts.
- `postgres/`: list queries and configuration stores; find exact run/delivery query definitions before changing them.

### Asset/CSP strategy

Vendor one pinned HTMX version locally, served through embedded static assets; record version/license. Keep custom CSS, no React/Tailwind/node runtime requirement. Serve small local CSS/JS assets and set only necessary CSP permissions: `script-src 'self'` plus required bootstrap hashes, `style-src 'self'` plus legacy inline style allowance while migrating, `connect-src 'self'`. Keep `default-src 'none'`, `base-uri 'none'`, `frame-ancestors 'none'`, normal `form-action 'self'`. Don't add `unsafe-eval`, blanket script `unsafe-inline`, CDN allowances, or remote fonts. Test CSP with Basic Auth enabled and insecure loopback mode.

Retain the early theme bootstrap to avoid flashes. Move event listeners to local JS with delegation, or update the two-script hash invariant deliberately with tests. Configure HTMX eval/script insertion off (`allowEval: false`, `allowScriptTags: false`) and use declarative triggers plus delegated events, not inline `hx-on`, `js:` headers, or eval-requiring trigger filters. Verify the pinned version supports this configuration. Static JS handles only theme, dialog lifecycle, focus, response errors, and bounded polling behavior.

### Stable render contracts

- Normal GET returns a full templ document; same route with `HX-Request: true` returns the specifically requested content/list/form fragment. Add `Vary: HX-Request` where content varies, and `HX-Target` if it influences representation. History-restore requests must obtain full documents when required by HTMX configuration.
- Factor reusable templ content and result regions; do not swap a complete `<html>` document into a table. Use stable IDs such as `page-content`, `models-results`, `runs-results`, `run-events`, `modal-root`, and `form-errors`.
- First enhance per-page search, forms, dependent selectors, and event refresh. Keep normal sidebar links initially. Add opt-in same-origin navigation enhancement only after title/history/active navigation/focus contracts are tested. Avoid blanket `hx-boost` on all forms or links.
- Native GET search form uses `q` and allowlisted filters. HTMX submits its containing form with debounce ~250ms, cancels outdated requests using `hx-sync`, swaps only results, and updates the URL without creating a history entry for every keystroke. Keep the input outside the results target so caret/focus survive.
- Query applies before database pagination. Do not search just the capped 100 rows; add parameterized filtered/paginated run/delivery queries and matching count queries or honest cursor counts. Clamp query length/page size, allowlist sort/status/type, stable tie-breaker, escape wildcard semantics if query is literal. Small configuration lists may filter server-side after existing reads when those reads are genuinely complete.
- Existing POST methods and hidden `csrf` fields remain; URL-encoded submissions and Origin checks are preserved. Apply mutation protections to **all** model/settings child delete routes as well as configuration/setup. HTMX is not an authorization boundary.
- Validation returns retained templ form with errors and appropriate 422; explicit JS response handling swaps only expected validation/conflict forms (e.g. 422/409), not arbitrary error bodies. Auth/CSRF/500/network failures produce accessible notices without losing user input. Successful native POST stays Post/Redirect/Get; enhanced success has explicit `HX-Redirect` to canonical local route or a fragment + refresh event, not an implicit 303 full-page swap. Validate any return URL against same-origin allowed paths.
- Download links and GitHub external/manifest/callback flows remain unboosted; external form-action policy belongs to a full-document response, not an HTMX fragment.
- Model endpoint fetches remain backend-only; UI adds no external connection probes.

## 7. Animation specification

Reference: [HTMX animations examples](https://htmx.org/examples/animations/) (reviewed during audit). Adapt the mechanisms, not the demo's one-second durations.

| Interaction | Mechanism | Duration/behavior |
| --- | --- | --- |
| Search result replacement | Stable target; `.htmx-swapping`, `.htmx-settling` opacity | 80ms outgoing, ~120ms incoming, no slide or input replacement |
| Request in flight | `.htmx-request`/indicator, `aria-busy` | Delayed subtle indicator; retain readable previous results |
| Dialog open/close | Native dialog + local delegated JS + CSS opacity/translateY | 160–180ms; close after animation, then restore opener focus |
| Content navigation | `hx-swap` `transition:true`, scoped `view-transition-name` | Optional 160–180ms crossfade; CSS fallback without API |
| Successful save | `.htmx-added` or status-message fade | ~120ms; status announced once |
| Delete after success | `.htmx-swapping` and matching swap delay | ~120ms, only after confirmed server success |
| Timeline refresh | Stable region; no repeated entrance animation | Preserve scroll and selection; small update indicator only |

Use targeted `opacity`/`transform` transitions, not `transition: all`, bouncing, or layout-property animation. Stable IDs preserve continuity. Tune swap/settle modifiers to match CSS duration; keep motion under 200ms for routine actions. For `prefers-reduced-motion: reduce`, disable transform/View Transition motion and use zero or near-zero swap delays too; disabling CSS alone must not leave artificial waiting. Unsupported View Transitions must degrade to normal HTMX swaps. No animation library/extensions required.

Native dialog requirements: labeled title, close/Cancel, Escape, focus containment, initial focus, opener restoration, no nested dialogs, viewport-constrained scroll, unsaved changes protection where relevant. Deletion dialog defaults focus to Cancel and names the resource/dependency restriction. Without JS, link to a real confirmation page and normal POST rather than bypassing confirmation.

## 8. Step-by-step implementation handoff

**Do not start these steps until the plan is approved.** Each step is one logical work unit; regenerate templ and test immediately after changes, then broaden verification. Preserve unrelated local work. Never commit or push unless separately requested.

### Step 0: Establish baseline and approved guide

1. Read `AGENTS.md`, `PLAN.md` sections 12/14, this plan, relevant source and existing tests. Check git status before changes and confirm the server matches the current checkout.
2. Run targeted `go test ./http/... ./web/...`, then existing broader baseline checks. If using Postgres tests, use an isolated test database/schema, not the populated capture database.
3. After approval, write the brand/style guide described in section 10; add a concise pointer and UI checklist to `AGENTS.md`. Update `PLAN.md` when approved decisions affect project milestones.
4. Keep capture assets read-only as baseline. If current records differ, record that rather than asserting pixel equality of sample data.

**Exit:** baseline results recorded; approved guide exists; no unrelated source changes.

### Step 1: Assets and hypermedia foundation

1. Add embedded local static serving, pinned HTMX, small application JS/CSS; no new frontend build framework.
2. Add minimal CSP sources and test script hashes/invariant, assets, Basic Auth, same-origin requests, no unsafe-eval.
3. Split page wrappers from content fragments without altering existing normal-GET rendering.
4. Define fragment target contracts, errors, Vary headers, success redirect behavior, delegated listeners, and focus/status region.

**Tests:** native/HTMX GET parity; fragment excludes duplicate shell; history restoration; auth/CSRF; malicious/unknown targets; CSP works in Chrome; GitHub external flow unchanged.

### Step 2: Shared components, spacing, theme

1. Add small templ components for page heading, panel, status badge, search toolbar, empty/no-match/error notice, field error, action row, pagination, dialog shell.
2. Replace ad hoc spacing with proposed tokens; keep route hierarchy and simple layout.
3. Replace cycling toggle with native System/Light/Dark control while preserving stored `auto`/light/dark values and early bootstrap. Add all semantic dark tokens.
4. Preserve mobile native navigation; add skip-to-content/focusable main and truthful `aria-current`.

**Tests:** theme persists navigation/reload, blocked localStorage safe, System follows OS change, no first-paint flash, focus/contrast at light/dark, 390/768/1440 widths and 200% zoom.

### Step 3: Searchable Models as the reference implementation

1. Add native GET query/filter parsing and shared Models results fragment with search debounce/cancellation.
2. Add readable name/ID/endpoint and legacy-default labeling; no health indicators.
3. Refactor model form essentials/advanced fields, error retention and success notices with identical validation/defaults.
4. Add New model dialog using the same form content, retaining full-page route; add confirmation fallback for deletion and preserve dependency checks.

**Tests:** name/ID search, Local/Hosted, no results versus no models, clearing filters, stale requests, query escaping, theme parity, invalid URL/key-env/token/concurrency input, duplicate save prevention, dialog Escape/focus/mobile scrolling, default/in-use deletion blocked. Use fakes, no real model calls.

### Step 4: Remaining automation lists and forms

1. Reuse toolbar/status/error patterns for Prompts, Agents, Workflows, Repositories, Schedules.
2. Keep dedicated editors, larger raw textareas, immutable fields/hidden values, pinned revision display and prerequisite notices.
3. Add workflow-type-dependent compatible agent selection fragment and accessible ordering; keep unsaved context.
4. Compact repository binding actions and return to detail after changes; preserve posting policy.
5. Keep schedule IANA/JSON validation and real next-occurrence display; no unsupported controls.

**Tests:** all create/edit/delete routes; fixed prompt kind, revision pinning, disabled selected dependency, incompatible workflow type, max four/distinct/order round-trip, bindings context, posting gates unchanged, cron/timezone/invalid JSON, schedule/history deletion restrictions. No real schedule activation in smoke tests.

### Step 5: Run/webhook list query and pagination

1. Find current store queries and all callers/interfaces; introduce backward-compatible filtered list methods or update fakes/callers deliberately.
2. Implement parameterized newest-first run/delivery search and allowlisted filters with stable pagination/count semantics. Add indexes only if query analysis warrants them, not by default.
3. Add results fragments, native links and HTMX query handling. Keep previous results during transient failure.
4. Replace neutral statuses with semantic badges using actual enums and provide clear native keyboard links.

**Tests:** fixture records beyond old cap searchable; filter-before-pagination, total/count truth, invalid page/size/filter, boundary pages, stable order, no query injection, encoded URLs and native-JS parity. Postgres integration uses isolated schema; no changes to production/read data.

### Step 6: Run detail, timeline, output

1. Refactor detail/partial result states; red failure surface; never turn failure into successful zero-findings result.
2. Add structured timeline fragment/search/wrap/manual refresh; preserve escaped multiline output.
3. Add active-run polling only, visibility pause/resume and user pause, terminal stop, error notice and preserved scroll/selection. Do not fabricate raw stdout.
4. Add state-aware artifact availability only when actual data/store can determine it; keep download routes unchanged.

**Tests:** queued/running/failed/completed/partial findings, scheduled no-output and multiline output, event text XSS escaping, polling transitions/errors/terminal stop, hidden-tab pause, no focus theft, safe missing artifacts.

### Step 7: Overview and GitHub onboarding

1. Balanced six-card directory and latest five actual run rows; no fake aggregate total.
2. Correct blanket dry-run copy without modifying gates.
3. Restyle setup/installation states and errors; suppress unavailable environment app ID/slug instead of fabricating it.
4. Ensure manifest external continuation/callback use full documents with the existing restricted external CSP and state validation.

**Tests:** empty/populated Overview; fewer than five runs; no invented totals; GitHub unconfigured/environment/manifest/installations/suspended states; error retention; no secrets in HTML; external continuation policy and callback flow unchanged (fakes only).

### Step 8: Motion, accessibility, final regression

1. Add section 7 transitions after response contracts work. Scoped transitions only; reduced-motion removes delays and animation.
2. Check every route family with keyboard, native forms without JS, 200% zoom, small viewport, System/light/dark, long identifiers/messages, empty/error/loading/disabled states.
3. Capture implemented screenshots in a new `docs/ui-improve/implemented/` folder. Compare composition with refined references, not fabricated data/pixel dimensions. Keep before/after gallery.
4. Validate no console/CSP/network errors and no live GitHub posting, model calls, or schedule changes during smoke tests.
5. Run repository generation/format/vet/lint/tests/build; report pre-existing unrelated failures separately. Mark plan steps complete only with evidence.

```sh
go generate ./...
gofmt -w .
go vet ./...
make lint
make test
make build
```

Generated `*_templ.go` outputs must remain in the eventual diff alongside `.templ` changes. Do not hand-edit generated templates. Browser testing may use existing installed Playwright or Chrome DevTools; adding a permanent JS test toolchain is a separate decision, not required for the Go/templ application runtime.

## 9. Definition of done

- [ ] Approved style guide referenced by `AGENTS.md`; no unapproved rebrand/framework migration.
- [ ] All captured page families and all existing CRUD routes remain usable with native navigation/forms.
- [ ] Search works against all relevant stored records, not only a capped visible subset; query state and truthful counts preserved.
- [ ] Model dialog and delete confirmation are keyboard-safe with full-page fallbacks.
- [ ] Errors preserve values, identify affected fields, announce status, and do not double-submit.
- [ ] Prompt identity/revision pinning, job kind compatibility, workflow ordering/distinctness and posting gates unchanged.
- [ ] Light/dark/System work across shell, tables, fields, notices, code, logs and dialogs without flashes.
- [ ] Responsive at 390/768/1440 and 200% zoom, no unintended body overflow.
- [ ] HTMX requests/auth/CSRF/CSP/history/download/external GitHub contracts verified.
- [ ] Motion follows the HTMX examples, under ~200ms, disabled along with delays for reduced motion.
- [ ] Run events readable/searchable; polling preserves reading position and stops appropriately; no raw-log claims.
- [ ] All tests/checks pass or unrelated baseline failures precisely documented; no live model/GitHub writes used as tests.
- [ ] Actual after screenshots recorded; no mockup-only controls, fabricated telemetry, or security claims shipped.

## 10. Post-approval style and brand guide

Create `docs/ui-style-guide.md` **after approval**, then reference it from `AGENTS.md` with requirements to load it before UI edits. Keep it concise enough for future agents but specific enough to prevent visual drift.

The approved guide should contain:

1. Brand: lowercase overload, existing teal o mark, straightforward operational voice; no marketing subtitle/rebrand by default.
2. Semantic light/dark token pairs and measured contrast; spacing, typography, radii, borders, density, breakpoints and touch targets.
3. Go templ component catalog and actual file locations; reuse rules, native fallback behavior, HTMX fragment IDs/contracts.
4. Form/search/list/dialog/status/log patterns with examples drawn from implemented components, not exported Tailwind.
5. Motion timings, reduced-motion behavior, focus/keyboard/error/empty/loading rules.
6. Product truth rules: no inferred health, fake telemetry, unsupported actions, credentials in UI, or misleading posting/final prompt claims.
7. Test/regeneration commands and screenshot review checklist.
8. Approved screenshots linked as references, identifying which aspects are authoritative and which sample data is not.

Approval should settle the restrained density, theme selector, model-create dialog scope, timeline-not-stdout interpretation, and whether literal image-conditioned Stitch generation is required. Do not overwrite existing engineering instructions when adding the pointer.

## 11. Existing and proposed screenshots

Every existing capture is indexed in the manifest and the gallery below. Proposed screenshots are Stitch-generated refined references. Not every list/create/edit state has an individually generated redesign: shared components and the page specifications define those states. Repository/workflow list pages use the Models/Agents list pattern; their focused edit reference shows their unique content.

### Overview

**Existing**

![Existing Overview](docs/ui-improve/existing/overview-light.png)

**Proposed Stitch reference**

![Proposed Overview](docs/ui-improve/stitch/overview-light-refined.png)

### Models, light and dark

**Existing light**

![Existing Models light](docs/ui-improve/existing/settings-light.png)

**Proposed light**

![Proposed Models light](docs/ui-improve/stitch/models-light-refined.png)

**Existing dark**

![Existing Models dark](docs/ui-improve/existing/settings-dark.png)

**Proposed dark**

![Proposed Models dark](docs/ui-improve/stitch/models-dark-refined.png)

### Model creation and modal

![Existing New model](docs/ui-improve/existing/settings-new-light.png)

![Proposed New model dialog](docs/ui-improve/stitch/model-dialog-light-refined.png)

### Runs

![Existing Runs dark](docs/ui-improve/existing/runs-dark.png)

![Proposed Runs dark](docs/ui-improve/stitch/runs-dark-refined.png)

### Run detail and timeline

![Existing failed run](docs/ui-improve/existing/runs-599-light.png)

![Proposed failed run and timeline](docs/ui-improve/stitch/run-detail-dark-refined.png)

### Prompts

![Existing Prompts](docs/ui-improve/existing/configure-prompts-light.png)

![Proposed Prompts editor pattern](docs/ui-improve/stitch/prompts-light-refined.png)

### Agents

![Existing Agents](docs/ui-improve/existing/configure-agents-light.png)

![Proposed Agents](docs/ui-improve/stitch/agents-light-refined.png)

### Workflows

![Existing Workflows](docs/ui-improve/existing/configure-workflows-light.png)

![Proposed Workflow editor](docs/ui-improve/stitch/workflow-light-refined.png)

### Repository bindings

![Existing repository detail](docs/ui-improve/existing/configure-repositories-212-light.png)

![Proposed repository detail](docs/ui-improve/stitch/repository-light-refined.png)

### Schedules

![Existing Schedules](docs/ui-improve/existing/configure-schedules-light.png)

![Proposed Schedules editor pattern](docs/ui-improve/stitch/schedules-light-refined.png)

### GitHub setup

![Existing GitHub setup](docs/ui-improve/existing/setup-github-light.png)

![Proposed GitHub setup](docs/ui-improve/stitch/github-light-refined.png)

### Webhooks

![Existing Webhooks](docs/ui-improve/existing/webhooks-light.png)

![Proposed Webhooks](docs/ui-improve/stitch/webhooks-light-refined.png)

### Additional existing forms and states

The remaining baseline captures are embedded for complete route/state coverage.

<details>
<summary>Run list, completed and scheduled run details</summary>

![Existing Runs light](docs/ui-improve/existing/runs-light.png)
![Existing run 597](docs/ui-improve/existing/runs-597-light.png)
![Existing run 588](docs/ui-improve/existing/runs-588-light.png)

</details>

<details>
<summary>Model edit and dark creation</summary>

![Existing hosted model edit](docs/ui-improve/existing/settings-crush-gpt-6-sol-light.png)
![Existing model creation dark](docs/ui-improve/existing/settings-new-dark.png)

</details>

<details>
<summary>Prompt creation and both edit kinds</summary>

![Existing prompt creation](docs/ui-improve/existing/configure-prompts-new-light.png)
![Existing entry prompt edit](docs/ui-improve/existing/configure-prompts-entry-bonsai-go-review-skill-entry-light.png)
![Existing review prompt edit](docs/ui-improve/existing/configure-prompts-review-demo-security-pass-light.png)

</details>

<details>
<summary>Agent creation and edit</summary>

![Existing agent creation](docs/ui-improve/existing/configure-agents-new-light.png)
![Existing agent edit](docs/ui-improve/existing/configure-agents-bonsai-go-review-skill-agent-light.png)

</details>

<details>
<summary>Repository list and creation</summary>

![Existing Repositories](docs/ui-improve/existing/configure-repositories-light.png)
![Existing repository creation](docs/ui-improve/existing/configure-repositories-new-light.png)

</details>

<details>
<summary>Workflow creation and edit</summary>

![Existing workflow creation](docs/ui-improve/existing/configure-workflows-new-light.png)
![Existing workflow edit](docs/ui-improve/existing/configure-workflows-bonsai-go-review-skill-workflow-light.png)

</details>

<details>
<summary>Schedule creation and edit</summary>

![Existing schedule creation](docs/ui-improve/existing/configure-schedules-new-light.png)
![Existing schedule edit](docs/ui-improve/existing/configure-schedules-demo-daily-summary-light.png)

</details>

<details>
<summary>Additional dark and mobile baselines</summary>

![Existing Overview dark](docs/ui-improve/existing/overview-dark.png)
![Existing Webhooks dark](docs/ui-improve/existing/webhooks-dark.png)
![Existing Models mobile light](docs/ui-improve/existing/settings-light-mobile.png)
![Existing Runs mobile dark](docs/ui-improve/existing/runs-dark-mobile.png)

</details>

### Refined Stitch screen IDs

| Reference | Screen ID |
| --- | --- |
| Overview light | `21509362cf0e4863b80204e375da75b0` |
| Models light | `d736d30cca44414b9817543c471c5904` |
| Models dark | `a93a1356ba0d4386b5b4bd971a3af01b` |
| New model dialog light | `3f8a9e805dad437391c07753865a4114` |
| Runs dark | `6ed580eaddb94454a20f464a1a9eef4f` |
| Run detail dark | `2aabae4e72f640fea64a7a5c9ec9231c` |
| Prompts light | `4a6d19d7d02444d797ae5063a1a02114` |
| Agents light | `3c079b0bd8044daaa960adc1a9e13145` |
| Workflow light | `d144646abbdd4da5b022ecf300c6a96f` |
| Repository light | `59f05572b3334a7a9dffc15a452bd2d8` |
| Schedules light | `196691f34754472791d0f1438953349e` |
| GitHub light | `18b42f8ad47f4099bd26a4b433356347` |
| Webhooks light | `d20cb675fe154eef8f24dad7d6066de1` |
