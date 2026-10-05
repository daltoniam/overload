package overload

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"
)

// SnapshotVersion is the format of resolved workflows pinned on runs.
// Version 0 snapshots predate scopes: every agent reviews every file.
// Version 2 added scopes and skip paths, version 3 the planner and verifier.
const SnapshotVersion = 3

// Main agent review modes.
const (
	MainReviewsAll       = "all"
	MainReviewsUnclaimed = "unclaimed"
)

const (
	maxGlobs          = 64
	maxGlobLength     = 200
	maxFindingsCap    = 50
	maxFileReviewsCap = 10000
	maxPreviewFiles   = 3000
)

// Scope modes. ScopeGlobs (the default) reviews files matching the paths
// and any the planner adds; ScopeAlways reviews only files matching the
// paths; ScopePlanned reviews only files the planner assigns.
const (
	ScopeGlobs   = "globs"
	ScopeAlways  = "always"
	ScopePlanned = "planned"
)

const maxDescription = 300

// Scope limits a sub-agent to the changed files it is for. No paths means
// every changed file, except in planned mode. The description tells the
// planner what the sub-agent is for.
type Scope struct {
	Paths       []string `json:"paths,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	Description string   `json:"description,omitempty"`
	MaxFindings int      `json:"max_findings,omitempty"`
}

func (scope Scope) Validate() error {
	if scope.MaxFindings < 0 || scope.MaxFindings > maxFindingsCap {
		return fmt.Errorf("max findings must be between 0 and %d", maxFindingsCap)
	}
	switch scope.Mode {
	case "", ScopeGlobs, ScopeAlways:
	case ScopePlanned:
		if len(scope.Paths) > 0 {
			return errors.New("planned scopes take files from the planner, not paths")
		}
	default:
		return errors.New("scope mode must be globs, always or planned")
	}
	if len(scope.Description) > maxDescription || strings.ContainsFunc(scope.Description, func(r rune) bool { return r < 32 }) {
		return fmt.Errorf("scope description must be one line of at most %d bytes", maxDescription)
	}
	return validateGlobs(scope.Paths)
}

func (scope Scope) empty() bool {
	return len(scope.Paths) == 0 && scope.MaxFindings == 0 && scope.Mode == "" && scope.Description == ""
}

// Plannable reports whether the planner may assign files to the scope.
func (scope Scope) Plannable() bool {
	return scope.Mode != ScopeAlways
}

// CompileGlob turns a path glob into a matcher. `*` and `?` stay within one
// path segment, `**` spans any number of segments, and a pattern without `/`
// matches a file name at any depth, so `*.lock` matches `web/package.lock`.
func CompileGlob(pattern string) (*regexp.Regexp, error) {
	if pattern == "" || len(pattern) > maxGlobLength || strings.HasPrefix(pattern, "/") || strings.ContainsAny(pattern, "\x00\r\n") {
		return nil, fmt.Errorf("invalid path glob %q", pattern)
	}
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	var expression strings.Builder
	expression.WriteString("^")
	for index := 0; index < len(pattern); index++ {
		switch {
		case strings.HasPrefix(pattern[index:], "**/"):
			expression.WriteString("(?:.*/)?")
			index += 2
		case strings.HasPrefix(pattern[index:], "**"):
			expression.WriteString(".*")
			index++
		case pattern[index] == '*':
			expression.WriteString("[^/]*")
		case pattern[index] == '?':
			expression.WriteString("[^/]")
		default:
			expression.WriteString(regexp.QuoteMeta(pattern[index : index+1]))
		}
	}
	expression.WriteString("$")
	return regexp.Compile(expression.String())
}

func validateGlobs(globs []string) error {
	if len(globs) > maxGlobs {
		return fmt.Errorf("at most %d path globs", maxGlobs)
	}
	for _, glob := range globs {
		if _, err := CompileGlob(glob); err != nil {
			return err
		}
	}
	return nil
}

type globSet []*regexp.Regexp

func compileGlobs(globs []string) (globSet, error) {
	set := make(globSet, 0, len(globs))
	for _, glob := range globs {
		matcher, err := CompileGlob(glob)
		if err != nil {
			return nil, err
		}
		set = append(set, matcher)
	}
	return set, nil
}

func (set globSet) match(path string) bool {
	for _, matcher := range set {
		if matcher.MatchString(path) {
			return true
		}
	}
	return false
}

// AgentFiles lists the changed files one agent reviews and, once a run has
// finished, what the agent reported. Planned lists the files the planner
// added beyond the agent's scope; Unreviewed the files it did not review
// because it failed.
type AgentFiles struct {
	Agent        string   `json:"agent"`
	Files        []string `json:"files"`
	Planned      []string `json:"planned,omitempty"`
	Reviewed     int      `json:"reviewed,omitempty"`
	Findings     int      `json:"findings,omitempty"`
	Capped       int      `json:"capped,omitempty"`
	Verified     int      `json:"verified,omitempty"`
	Dropped      int      `json:"dropped,omitempty"`
	InputTokens  int64    `json:"input_tokens,omitempty"`
	OutputTokens int64    `json:"output_tokens,omitempty"`
	Failed       string   `json:"failed,omitempty"`
	Unreviewed   []string `json:"unreviewed,omitempty"`
}

// Routing records which agent reviews which changed file. Agents are in
// workflow order, main agent first. Planner describes what the planner did,
// and Degraded lists why a completed review is partial.
type Routing struct {
	Skipped  []string     `json:"skipped,omitempty"`
	Agents   []AgentFiles `json:"agents"`
	Planner  string       `json:"planner,omitempty"`
	Degraded []string     `json:"degraded,omitempty"`
}

// FileReviews is the number of (agent, file) reviews the routing needs.
func (routing Routing) FileReviews() int {
	total := 0
	for _, agent := range routing.Agents {
		total += len(agent.Files)
	}
	return total
}

// Assignment gives one changed file to one sub-agent.
type Assignment struct {
	Path  string
	Agent string
}

// Route assigns changed files to agents: files matching skip paths are
// dropped, each sub-agent gets the files its scope matches, and the main
// agent gets every file or only the files no sub-agent claimed.
func (workflow ResolvedWorkflow) Route(paths []string) (Routing, error) {
	return workflow.route(paths, nil)
}

func (workflow ResolvedWorkflow) route(paths []string, planned []Assignment) (Routing, error) {
	skip, err := compileGlobs(workflow.SkipPaths)
	if err != nil {
		return Routing{}, err
	}
	scopes := make([]globSet, len(workflow.Agents))
	extra := make([]map[string]bool, len(workflow.Agents))
	index := map[string]int{}
	for position, agent := range workflow.Agents {
		if scopes[position], err = compileGlobs(agent.Scope.Paths); err != nil {
			return Routing{}, err
		}
		extra[position] = map[string]bool{}
		index[agent.Name] = position
	}
	for _, assignment := range planned {
		if position, ok := index[assignment.Agent]; ok && position > 0 {
			extra[position][assignment.Path] = true
		}
	}
	routing := Routing{Agents: make([]AgentFiles, len(workflow.Agents))}
	for position, agent := range workflow.Agents {
		routing.Agents[position] = AgentFiles{Agent: agent.Name, Files: []string{}}
	}
	for _, path := range paths {
		if skip.match(path) {
			routing.Skipped = append(routing.Skipped, path)
			continue
		}
		claimed := false
		for position := 1; position < len(workflow.Agents); position++ {
			byGlob := workflow.Agents[position].Scope.Mode != ScopePlanned && (len(scopes[position]) == 0 || scopes[position].match(path))
			if byGlob || extra[position][path] {
				routing.Agents[position].Files = append(routing.Agents[position].Files, path)
				claimed = true
			}
			if !byGlob && extra[position][path] {
				routing.Agents[position].Planned = append(routing.Agents[position].Planned, path)
			}
		}
		if len(workflow.Agents) > 0 && (workflow.MainReviews != MainReviewsUnclaimed || !claimed) {
			routing.Agents[0].Files = append(routing.Agents[0].Files, path)
		}
	}
	if limit := workflow.MaxFileReviews; limit > 0 && routing.FileReviews() > limit {
		return routing, fmt.Errorf("routing needs %d file reviews, more than the workflow limit of %d", routing.FileReviews(), limit)
	}
	return routing, nil
}

// HasPlannableAgents reports whether a planner could assign files to any
// sub-agent of the workflow.
func (workflow ResolvedWorkflow) HasPlannableAgents() bool {
	for _, agent := range workflow.Agents[min(1, len(workflow.Agents)):] {
		if agent.Scope.Plannable() {
			return true
		}
	}
	return false
}

// ApplyPlan adds a planner's assignments ({"path": ["sub-agent", ...]}) to
// the routing of paths. The planner can only add files: assignments of
// unknown or skipped files, to unknown sub-agents, the main agent or
// sub-agents in always mode reject the whole plan, and the glob routing is
// returned with the error. When the plan would exceed the file-review
// limit, planner assignments are dropped from the end until it fits; glob
// assignments are never dropped. An error from glob routing itself is
// returned as is.
func (workflow ResolvedWorkflow) ApplyPlan(paths []string, plan map[string][]string) (Routing, error) {
	base, err := workflow.Route(paths)
	if err != nil {
		return base, err
	}
	reviewable := map[string]bool{}
	for _, path := range paths {
		reviewable[path] = true
	}
	for _, path := range base.Skipped {
		reviewable[path] = false
	}
	agents := map[string]int{}
	for position, agent := range workflow.Agents {
		if position > 0 && agent.Scope.Plannable() {
			agents[agent.Name] = position
		}
	}
	assigned := make([]map[string]bool, len(workflow.Agents))
	for position, files := range base.Agents {
		assigned[position] = map[string]bool{}
		for _, path := range files.Files {
			assigned[position][path] = true
		}
	}
	wanted := map[Assignment]bool{}
	for path, names := range plan {
		if !reviewable[path] {
			return base, fmt.Errorf("plan names a file that is not reviewed: %q", TruncateUTF8(path, 200))
		}
		for _, name := range names {
			if _, ok := agents[name]; !ok {
				return base, fmt.Errorf("plan names an unknown or unplannable sub-agent: %q", TruncateUTF8(name, 100))
			}
			wanted[Assignment{Path: path, Agent: name}] = true
		}
	}
	var additions []Assignment
	for _, path := range paths {
		for position, agent := range workflow.Agents {
			if wanted[Assignment{Path: path, Agent: agent.Name}] && !assigned[position][path] {
				additions = append(additions, Assignment{Path: path, Agent: agent.Name})
			}
		}
	}
	for keep := len(additions); keep > 0; keep-- {
		routing, err := workflow.route(paths, additions[:keep])
		if err != nil {
			continue
		}
		routing.Planner = fmt.Sprintf("planner added %d file reviews", keep)
		if dropped := len(additions) - keep; dropped > 0 {
			routing.Planner += fmt.Sprintf("; %d more dropped to stay within the file-review limit", dropped)
		}
		return routing, nil
	}
	base.Planner = "planner added no file reviews"
	if len(additions) > 0 {
		base.Planner = fmt.Sprintf("planner assignments dropped to stay within the file-review limit (%d)", len(additions))
	}
	return base, nil
}

// ServerLoad is the review work one model server receives.
type ServerLoad struct {
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	Local    bool   `json:"local"`
	Reviews  int    `json:"reviews"`
	Parallel int    `json:"parallel"`
}

// Rough time for one file review, used only for previews. Local numbers are
// from reasoning models on Apple Silicon; hosted models are much faster.
const (
	localReviewEstimate  = 8 * time.Minute
	hostedReviewEstimate = time.Minute
)

// Estimate is a rough wall-clock time for the server's reviews: batches of
// Parallel reviews, one after another.
func (load ServerLoad) Estimate() time.Duration {
	if load.Reviews == 0 {
		return 0
	}
	per := hostedReviewEstimate
	if load.Local {
		per = localReviewEstimate
	}
	batches := (load.Reviews + max(load.Parallel, 1) - 1) / max(load.Parallel, 1)
	return time.Duration(batches) * per
}

// Preview shows what a workflow would do with a list of changed files
// before any model is called.
type Preview struct {
	Routing  Routing       `json:"routing"`
	Servers  []ServerLoad  `json:"servers"`
	Calls    int           `json:"model_calls"`
	Estimate time.Duration `json:"-"`
	Minutes  int           `json:"estimate_minutes"`
	Warnings []string      `json:"warnings,omitempty"`
}

// Preview routes paths and estimates the work per model server. Servers run
// at the same time, so the estimate is the slowest server's.
func (workflow ResolvedWorkflow) Preview(paths []string) (Preview, error) {
	routing, err := workflow.Route(paths)
	preview := Preview{Routing: routing, Servers: workflow.ServerLoads(routing), Calls: routing.FileReviews()}
	if err != nil {
		preview.Warnings = append(preview.Warnings, err.Error())
	}
	localModels := map[string]bool{}
	for _, load := range preview.Servers {
		preview.Estimate = max(preview.Estimate, load.Estimate())
		if load.Local {
			localModels[load.Model] = true
		}
	}
	preview.Minutes = int(preview.Estimate / time.Minute)
	if len(localModels) > 1 {
		preview.Warnings = append(preview.Warnings, fmt.Sprintf("%d different local models must be loaded at the same time; they are unlikely to fit in memory together unless the machine has room for all of them", len(localModels)))
	}
	planner := workflow.PlannerPrompt != nil && workflow.HasPlannableAgents()
	for position, agent := range routing.Agents {
		switch {
		case len(agent.Files) > 0 || len(paths) == 0:
		case planner && position > 0 && workflow.Agents[position].Scope.Plannable():
			preview.Warnings = append(preview.Warnings, fmt.Sprintf("%s reviews only files the planner assigns", agent.Agent))
		default:
			preview.Warnings = append(preview.Warnings, fmt.Sprintf("%s reviews no files", agent.Agent))
		}
	}
	if planner {
		preview.Calls++
		preview.Warnings = append(preview.Warnings, "The planner runs once per review and may add files for sub-agents not in always mode, so the review can take longer than shown.")
	}
	if workflow.VerifierPrompt != nil {
		preview.Warnings = append(preview.Warnings, "The verifier adds one model call per sub-agent finding.")
	}
	return preview, err
}

// ServerKey identifies a model server. Agents with the same key share one
// parallel limit, the smallest Concurrency among them.
func (model ModelProfile) ServerKey() string {
	return model.BaseURL + "\x00" + model.Model
}

// ServerLoads groups a routing's reviews by model server in first-use order.
func (workflow ResolvedWorkflow) ServerLoads(routing Routing) []ServerLoad {
	var loads []ServerLoad
	index := map[string]int{}
	for position, agent := range workflow.Agents {
		key := agent.Model.ServerKey()
		load, ok := index[key]
		if !ok {
			load = len(loads)
			index[key] = load
			loads = append(loads, ServerLoad{BaseURL: agent.Model.BaseURL, Model: agent.Model.Model, Local: agent.Model.ConnectionKind != "hosted", Parallel: max(agent.Model.Concurrency, 1)})
		}
		loads[load].Parallel = min(loads[load].Parallel, max(agent.Model.Concurrency, 1))
		if position < len(routing.Agents) {
			loads[load].Reviews += len(routing.Agents[position].Files)
		}
	}
	return loads
}

// validateRouting checks the routing settings shared by stored and resolved
// workflows.
type routingSettings struct {
	kind              string
	skipPaths         []string
	mainReviews       string
	maxFileReviews    int
	scopes            map[string]Scope
	planner, verifier bool
}

func validateRouting(settings routingSettings) error {
	kind, skipPaths, mainReviews, maxFileReviews, scopes := settings.kind, settings.skipPaths, settings.mainReviews, settings.maxFileReviews, settings.scopes
	if kind != "pr_review" && (len(skipPaths) > 0 || (mainReviews != "" && mainReviews != MainReviewsAll) || maxFileReviews != 0 || len(scopes) > 0 || settings.planner || settings.verifier) {
		return errors.New("only PR review workflows have scopes, skip paths, review limits, a planner or a verifier")
	}
	for name, scope := range scopes {
		if scope.Mode == ScopePlanned && !settings.planner {
			return fmt.Errorf("scope for %s: planned scopes need a planner prompt", name)
		}
	}
	if mainReviews != "" && mainReviews != MainReviewsAll && mainReviews != MainReviewsUnclaimed {
		return errors.New("main_reviews must be all or unclaimed")
	}
	if maxFileReviews < 0 || maxFileReviews > maxFileReviewsCap {
		return fmt.Errorf("max_file_reviews must be between 0 and %d", maxFileReviewsCap)
	}
	if err := validateGlobs(skipPaths); err != nil {
		return err
	}
	for name, scope := range scopes {
		if err := scope.Validate(); err != nil {
			return fmt.Errorf("scope for %s: %w", name, err)
		}
	}
	return nil
}

// ParseChangedFiles reads a pasted list of changed files, one per line, for
// a preview. Blank lines and repeats are ignored.
func ParseChangedFiles(text string) ([]string, error) {
	var paths []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		path := strings.TrimSpace(line)
		if path == "" || seen[path] {
			continue
		}
		if !fs.ValidPath(path) || strings.ContainsRune(path, 0) {
			return nil, fmt.Errorf("invalid changed file path %q", path)
		}
		if len(paths) == maxPreviewFiles {
			return nil, fmt.Errorf("at most %d changed files", maxPreviewFiles)
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths, nil
}
