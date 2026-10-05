# AGENTS.md

overload reviews GitHub pull requests (and runs other single-shot agent jobs)
with models you run yourself. The primary target is a Mac with Apple Silicon
running a local model on Metal (DwarfStar or llama.cpp); Postgres + River hold
state and jobs, and the UI is templ. Kubernetes with Agent Sandbox is the team
deployment.

- The plan and roadmap are in [PLAN.md](PLAN.md) (section 12). Keep it updated
  when decisions change; user-facing setup lives in
  [deploy/README.md](deploy/README.md).
- Follow the engineering conventions in PLAN.md section 14.
- Before UI work, read [docs/ui-style-guide.md](docs/ui-style-guide.md).
  Follow its brand, light/dark tokens, spacing, accessibility, native fallback,
  HTMX response, and motion rules. The approved handoff and screenshots are in
  [ui-improve.md](ui-improve.md).
- Reuse templ components; keep Go, templ, custom CSS, and HTMX. Mockups must
  not introduce unsupported actions, fabricated telemetry, credential exposure,
  or changes to posting and prompt revision policy.
- Verify UI in light/dark/System, reduced motion, keyboard/no-JS paths, and
  mobile layouts; regenerate templates and run relevant tests.
- Before pushing: `make ci` (generate, gofmt, vet, lint, test, build). On a
  Mac, `make install-test` checks the installer end to end.
- UI templates follow the layout of
  [switchboard's templates](https://github.com/daltoniam/switchboard/tree/main/web/templates)
  (layouts, components, pages). Run `go generate ./...` after editing `.templ`
  files and commit the generated `*_templ.go` files.
