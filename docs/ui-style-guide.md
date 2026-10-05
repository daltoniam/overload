# Overload UI style and brand guide

Approved direction: 2026-10-04. Read this guide before changing UI. The implementation handoff is [ui-improve.md](../ui-improve.md); its refined screenshots illustrate composition, not backend capabilities or sample data. This guide governs future UI work, but does not claim the planned components already exist.

## Identity and product truth

- Brand is lowercase **overload**, with the existing rounded teal square and lowercase white `o`. Keep the simple sidebar, page header, tables, and focused forms. No rebrand, marketing subtitle, decorative dashboard, gradient, glass, or avatar system. The approved statistics dashboard uses real database aggregates: daily run volume, current run statuses, and job mix over 14 UTC calendar days. Include labeled values and a native data-table alternative, never fabricate zeroes on query failure.
- Navigation icons are local inline SVG from `web/templates/layouts/icons.templ`, 18px with consistent strokes and visible text labels. Decorative SVG is hidden from assistive technology. Page and chart entrance motion is 180ms, hover motion 120–140ms; all is disabled for reduced motion. Charts work without JavaScript or external chart/font libraries.
- Voice is direct and operational: “New model”, “Save workflow”, “No matching runs”, “GitHub credentials unavailable to worker”. Explain consequences and recovery, not implementation jargon or promises.
- Preserve Go, templ, custom CSS, and progressive enhancement with HTMX. No React, Tailwind migration, runtime Node requirement, animation framework, remote fonts, or icon-font service.
- Show persisted or explicitly computed facts only. Never infer connection health from a configured model, call a capped list the total database count, or copy fabricated IDs/latency/runner hosts from mockups.
- Preserve policy and validation. Posting requires repository opt-in, the server gate, and GitHub App. Enabling one checkbox is not proof posting is active. Do not introduce retry, run-now, cancel, webhook replay, or model testing controls without separately approved backend work.
- Model credentials are environment variable **names**, never secret values. GitHub manifest credentials are stored in Postgres; do not claim that no secrets are persisted. Credentials must not appear in rendered HTML, logs, screenshots, or generated design prompts.
- Current prompts are `entry` and `review`. Migration 015 removes final prompts. Do not offer final selectors or claim final synthesis execution. Job types are `pr_review` and `scheduled_prompt`; run statuses include `superseded` as well as queued/running/completed/failed. Use current source enums, not screenshot labels.
- Run logs mean persisted timeline events, not raw stdout. Current events have `At`, `Level`, `Step`, `Message`; a level may be displayed when actually present. Do not fabricate severity, worker identity, exit code, or streaming infrastructure.

## Color and theme

Keep one semantic variable system. Existing `data-theme` and `localStorage['overload-theme']` remain the source of preference. Display **System / Light / Dark**, preserving the existing stored `auto` value for System. System follows OS preference, including changes while open. Explicit preference wins. Storage failure must be harmless.

| Role | Light | Dark |
| --- | --- | --- |
| Canvas | `#f5f7f8` | `#111c26` |
| Surface | `#ffffff` | `#1c2a35` |
| Primary text | `#172635` | `#e6edf2` |
| Muted text | `#627282` | `#b1c1cd` |
| Structural border | `#dce4e9` | `#384958` |
| Accent/link/focus | `#176b66` | `#78d5c0` |
| Accent soft/selection | `#e6f3ef` | `#213d3d` |
| Input boundary | `#627282` | `#b1c1cd` |
| Danger text | `#b42334` | `#f18491` |
| Danger soft | `#fff1f3` | `#3b252e` |
| Success text | `#176b66` | `#78d5c0` |
| Success soft | `#e6f3ef` | `#213d3d` |
| Warning text | `#854d0e` | `#facc15` |
| Warning soft | `#fffbeb` | `#352d1b` |
| Info text | `#1d4ed8` | `#93c5fd` |
| Info soft | `#eff6ff` | `#1e3048` |
| Code background | `#eef3f5` | `#304451` |
| Primary button background | `#176b66` | `#78d5c0` |
| Primary button text | `#ffffff` | `#10251f` |
| Destructive button background | `#b42334` | `#f18491` |
| Destructive button text | `#ffffff` | `#251018` |

Borders separating panels are decorative and may be low contrast. Inputs and meaningful control boundaries use the stronger boundary token, not the decorative border. Pair additional hover, disabled, backdrop, and field tokens in both themes; no hardcoded light-only surfaces. Code/log text and backgrounds follow theme, not a permanently dark `pre` style.

Measure actual combinations: normal text >=4.5:1, large text >=3:1, meaningful focus/control boundaries >=3:1. Color never replaces the status label. Do not use reduced opacity on an entire panel to convey loading if it makes text illegible.

Apply theme before first paint with the existing early bootstrap, retaining its CSP hash or deliberately updating its tested policy. Theme controls survive partial swaps and never reset during navigation.

## Typography, geometry, spacing

| Element | Specification |
| --- | --- |
| Body | System UI stack, 14–15px, ~1.5 line-height |
| Page title | 28–32px, semibold/bold, restrained negative tracking |
| Section heading | 18–20px |
| Secondary text | 12–13px, readable contrast |
| Technical text | `ui-monospace, SFMono-Regular, Menlo, Consolas, monospace` |
| Spacing scale | 4, 8, 12, 16, 24, 32, 48px |
| Sidebar | 238px desktop, retain existing destinations/order |
| Main content | ~1160px maximum, 32–48px desktop gutter, 16px mobile |
| Form | ~720px maximum; avoid ultrawide inputs |
| Dialog | ~640px maximum, viewport-constrained height and body scrolling |
| Panel padding/gap | 20–24px padding, 16–24px inter-panel spacing |
| Controls | ~40px visual height, >=44px mobile touch target |
| Table rows | >=48px, ~56px for secondary metadata; grow for wrapping |
| Radius | 8px controls, 12px panels/dialogs; restrained badge rounding |

Use monospace for IDs, endpoints, SHAs, timestamps, JSON, cron, and prompt content, not all labels/navigation. Preserve line breaks in user/model output with escaped text and `white-space: pre-wrap`; do not turn it into trusted HTML.

Panels rely on hairline borders and tonal surfaces, not heavy shadows. Only overlays need a small shadow and non-blurred backdrop. Allow whitespace rather than filling it with invented telemetry.

At ~780px collapse the sidebar into a compact header with native Navigation disclosure. Stack toolbars/field columns. Tables may scroll inside a labeled wrapper; the body must not overflow. At 390px and 200% zoom actions remain reachable, long identifiers wrap, and dialog footer is not clipped. Avoid icon-only navigation that requires a tooltip framework.

## Navigation and shared patterns

Retain order: Overview, Runs, Models, Prompts, Agents, Repositories, Workflows, Schedules, GitHub, Webhooks. Child pages keep their parent's active state. Provide a skip link, labeled main region, one page-level heading, and meaningful native links.

### Lists and search

- One page title, short description, one primary create action when supported; one panel containing toolbar/results/footer.
- Search has a visible or accessible label. Use native GET form, native selects, clear filters, and truthful count. Empty collection and no matches have different messages and recovery links.
- Resource names are links. Secondary endpoint/model metadata is quiet and readable; show default badges only where true and clarify legacy-review meaning.
- Search input is outside the swapped results region so caret/focus survive. Debounce ~250ms, cancel superseded requests, retain prior results while loading. Announce result count politely, not every row.
- Runs/webhooks filter before database pagination, not after loading the first 100. Parameterize queries, allowlist filters/sorts, bound query/page size, and include stable ID ordering.

### Forms and dependent controls

- Group essentials first; advanced options in native disclosure. Keep all existing field bounds/defaults and submitted identity/CSRF fields.
- Use persistent labels, linked helper text, `aria-invalid` plus associated inline errors. Validation keeps entered values and focuses an error summary/first invalid field.
- Save is primary; Cancel is an explicit native link/button with clear destination. Disable duplicate submissions only while a request is outstanding; restore after failure.
- Readonly existing names/types reflect backend constraints. Show missing prerequisites with links. Preserve pinned revisions; saving an agent must not silently imply it retained a revision if backend pins latest.
- Workflow type changes refresh compatible agent options server-side without dropping unrelated input. Maximum four distinct ordered agents, accessible up/down controls, no graph/drag library and no guaranteed sequential-execution claim.
- Repository bindings use compact table and side-by-side actions; stay on repository detail after mutation. Danger zone is separate and dependency restrictions remain server-enforced.

### Dialogs and confirmation

- Use native `<dialog>`, label/title, Close/Cancel, Escape, focus containment, safe initial focus, and opener focus restoration.
- Model creation can enhance its real full-page form route; complex editors remain dedicated pages unless explicitly approved.
- Destructive confirmation names the resource and impact, defaults focus to Cancel, and has a native confirmation-page fallback. Never make no-JS behavior delete immediately because a dialog could not open.
- Scroll body within viewport, retain accessible footer, protect unsaved input where relevant, and avoid nested dialogs.

### Status, errors, and logs

Queued/disabled/superseded are labeled neutral states; running uses accent; completed uses success; failed uses danger. Warning is not teal success. Render unknown future states truthfully with neutral styling.

Queued/running results are pending, not successful zero findings. Failed results may be partial; do not hide persisted findings. Completed zero findings retain the caveat that the PR is not proven bug-free.

Timeline rows have timestamp, actual level if useful, step, message. Include search, wrap, manual refresh; active polling only at ~3–5s with pause/resume, hidden-tab suspension, terminal stop, preserved scroll/selection, and visible refresh errors. Never reanimate every row or force-scroll a reader. External artifact downloads remain normal links.

GitHub setup is a full-document external manifest/callback flow, not a modal fragment. Show only available app metadata, never ID 0 as a real identity. Privacy copy must match current persistence.

## HTMX and motion

Reference: [HTMX animations](https://htmx.org/examples/animations/). Pin/vendor HTMX locally; serve embedded local assets. Disable eval and inserted scripts; avoid inline `hx-on`, `js:` headers, and eval-dependent trigger filters. Use small delegated local JS only for lifecycle/focus/theme/errors/polling.

Normal GET returns full templ page; enhanced GET returns the explicitly requested fragment, with correct `Vary` and history-restore behavior. Stable target IDs include `page-content`, resource `*-results`, `run-events`, `modal-root`, and `form-errors`. Do not nest full documents in fragments. Preserve auth, hidden POST-body CSRF, Origin/host guards, and request bounds. Validation 422/conflict 409 swaps only expected retained forms; network/auth/CSRF/server errors must not erase input. Enhanced success needs explicit redirect or refresh contract, not an accidental 303 full-document swap.

CSP permits only necessary local script/style sources and `connect-src 'self'`; retain restrictive defaults, no unsafe-eval or blanket script unsafe-inline. Existing theme hashes/invariant and GitHub external form-action policy require tests. Do not boost downloads, external GitHub navigation, or every mutation form blindly.

| Motion | Timing and mechanism |
| --- | --- |
| Search results | ~80ms outgoing + 120ms incoming opacity, stable target, swapping/settling classes |
| Dialog | 160–180ms opacity + small translateY, restore focus after close |
| Navigation | Optional scoped View Transition crossfade, 160–180ms, normal swap fallback |
| Success notice/new item | ~120ms fade using added/settling classes |
| Confirmed successful delete | ~120ms removal; matching swap delay |
| Polled events | No repeated row entrance motion; small update indicator only |

Use targeted opacity/transform, never `transition: all`, bounce, throbbing, or animation of table geometry. For reduced motion disable transforms/View Transitions **and** matching HTMX delays; disabled CSS must not leave artificial waiting. Set `aria-busy`, unobtrusive indicators, and accessible status independently of motion.

## Component ownership and source map

Current handwritten templates:

- `web/templates/layouts/base.templ`: shell, CSS, theme bootstrap/control.
- `web/templates/pages/models.templ`: Models list/form.
- `web/templates/pages/pages.templ`: Overview, Runs, detail, Webhooks.
- `web/templates/pages/automation_crud.templ`: prompts/workflows/schedules.
- `web/templates/pages/configuration_crud.templ`: agents/repositories/bindings.
- `web/templates/pages/github.templ`: setup/installations/manifest continuation.
- `http/`: rendering, validation, auth/CSRF/host guards, CSP.
- `postgres/`: filtering/pagination and pinned configuration reads.

Implemented shared components live in `web/templates/pages/ui.templ`: `SearchToolbar`, `ResultsFooter`, `ValidationError`, `Confirmation`, `WorkflowAgents`, and `RunEvents`. `web/assets/ui.css` supplies semantic styles and transitions; `web/assets/ui.js` owns delegated theme/dialog/error/polling lifecycle. HTMX 2.0.11 and its license are vendored in `web/assets/`. `http/ui.go` composes list/form fragments and native fallbacks, while `postgres/ui_lists.go` implements parameterized pagination. Reuse these actual components; other component names in the plan are proposed responsibilities, not existing APIs. Do not paste Stitch-exported Tailwind or generated HTML into source.

## Verification checklist

1. Read current source and inspect git status; the repository has concurrent unrelated work. Reconcile route/model drift without reverting others' changes.
2. Regenerate templ after each template change, test the changed handlers/components, then run broader repository checks. Keep generated output in eventual diffs; do not hand-edit it or commit unless asked.
3. Test native/no-JS and HTMX paths, keyboard/focus/escape, empty/loading/error/dependency cases, long/multiline content, 390/768/1440 widths, 200% zoom, System/light/dark, reduced motion, auth/CSRF/CSP/history.
4. Use fakes and isolated Postgres schema/database. Do not enable schedules/posting or make live model/GitHub writes for visual tests.
5. Record implemented screenshots separately from baseline/refined concepts and review hierarchy/spacing, not mockup sample-data pixel equality.

```sh
go generate ./...
go test ./http/... ./web/...
gofmt -w .
go vet ./...
make lint
make test
make build
```

Authoritative visual references: [Models light](ui-improve/stitch/models-light-refined.png), [Models dark](ui-improve/stitch/models-dark-refined.png), [model dialog](ui-improve/stitch/model-dialog-light-refined.png), [run timeline](ui-improve/stitch/run-detail-dark-refined.png). All references still yield to product truth, accessible typography, and this guide. See the plan for full gallery and known mockup inaccuracies.
