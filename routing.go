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
const SnapshotVersion = 2

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

// Scope limits a sub-agent to the changed files it is for. No paths means
// every changed file.
type Scope struct {
	Paths       []string `json:"paths,omitempty"`
	MaxFindings int      `json:"max_findings,omitempty"`
}

func (scope Scope) Validate() error {
	if scope.MaxFindings < 0 || scope.MaxFindings > maxFindingsCap {
		return fmt.Errorf("max findings must be between 0 and %d", maxFindingsCap)
	}
	return validateGlobs(scope.Paths)
}

func (scope Scope) empty() bool {
	return len(scope.Paths) == 0 && scope.MaxFindings == 0
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
// finished, what the agent reported.
type AgentFiles struct {
	Agent        string   `json:"agent"`
	Files        []string `json:"files"`
	Reviewed     int      `json:"reviewed,omitempty"`
	Findings     int      `json:"findings,omitempty"`
	Capped       int      `json:"capped,omitempty"`
	InputTokens  int64    `json:"input_tokens,omitempty"`
	OutputTokens int64    `json:"output_tokens,omitempty"`
}

// Routing records which agent reviews which changed file. Agents are in
// workflow order, main agent first.
type Routing struct {
	Skipped []string     `json:"skipped,omitempty"`
	Agents  []AgentFiles `json:"agents"`
}

// FileReviews is the number of (agent, file) reviews the routing needs.
func (routing Routing) FileReviews() int {
	total := 0
	for _, agent := range routing.Agents {
		total += len(agent.Files)
	}
	return total
}

// Route assigns changed files to agents: files matching skip paths are
// dropped, each sub-agent gets the files its scope matches, and the main
// agent gets every file or only the files no sub-agent claimed.
func (workflow ResolvedWorkflow) Route(paths []string) (Routing, error) {
	skip, err := compileGlobs(workflow.SkipPaths)
	if err != nil {
		return Routing{}, err
	}
	scopes := make([]globSet, len(workflow.Agents))
	for index, agent := range workflow.Agents {
		if scopes[index], err = compileGlobs(agent.Scope.Paths); err != nil {
			return Routing{}, err
		}
	}
	routing := Routing{Agents: make([]AgentFiles, len(workflow.Agents))}
	for index, agent := range workflow.Agents {
		routing.Agents[index] = AgentFiles{Agent: agent.Name, Files: []string{}}
	}
	for _, path := range paths {
		if skip.match(path) {
			routing.Skipped = append(routing.Skipped, path)
			continue
		}
		claimed := false
		for index := 1; index < len(workflow.Agents); index++ {
			if len(scopes[index]) == 0 || scopes[index].match(path) {
				routing.Agents[index].Files = append(routing.Agents[index].Files, path)
				claimed = true
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
	for _, agent := range routing.Agents {
		if len(agent.Files) == 0 && len(paths) > 0 {
			preview.Warnings = append(preview.Warnings, fmt.Sprintf("%s reviews no files", agent.Agent))
		}
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
func validateRouting(kind string, skipPaths []string, mainReviews string, maxFileReviews int, scopes map[string]Scope) error {
	if kind != "pr_review" && (len(skipPaths) > 0 || (mainReviews != "" && mainReviews != MainReviewsAll) || maxFileReviews != 0 || len(scopes) > 0) {
		return errors.New("only PR review workflows have scopes, skip paths or review limits")
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

// KeepRouting carries routing settings over from the stored version of a
// workflow when it is saved from a form that does not edit them. Scopes are
// kept only for agents that are still sub-agents.
func (workflow *Workflow) KeepRouting(previous Workflow) {
	if previous.Name != workflow.Name || workflow.Kind != "pr_review" || previous.Kind != "pr_review" {
		return
	}
	workflow.SkipPaths, workflow.MainReviews, workflow.MaxFileReviews = previous.SkipPaths, previous.MainReviews, previous.MaxFileReviews
	workflow.Scopes = nil
	for index, name := range workflow.Agents {
		if scope, ok := previous.Scopes[name]; ok && index > 0 {
			if workflow.Scopes == nil {
				workflow.Scopes = map[string]Scope{}
			}
			workflow.Scopes[name] = scope
		}
	}
}
