package overload

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Tiers sort pull requests by size and the paths they touch, without a
// model, so trivial changes cost nothing and small ones a lighter review.
// A file matching SensitivePaths always gets the full workflow. Files the
// workflow skips do not count.
type Tiers struct {
	SensitivePaths []string `json:"sensitive_paths,omitempty"`
	Trivial        *Tier    `json:"trivial,omitempty"`
	Lite           *Tier    `json:"lite,omitempty"`
}

// Tier is a size limit: at most MaxLines changed lines (added plus
// removed) in at most MaxFiles files. A trivial tier either skips the
// review (Action "skip") or reviews with Agents only; a lite tier reviews
// with Agents only. Agents name workflow agents ("main" is the main
// agent), which must include the main agent.
type Tier struct {
	MaxLines int      `json:"max_lines"`
	MaxFiles int      `json:"max_files"`
	Action   string   `json:"action,omitempty"`
	Agents   []string `json:"agents,omitempty"`
}

// Tier names.
const (
	TierTrivial = "trivial"
	TierLite    = "lite"
	TierFull    = "full"
)

// TierSkip is the trivial tier's action that skips the review.
const TierSkip = "skip"

// FileChange is one changed file's path and changed lines.
type FileChange struct {
	Path  string
	Lines int
}

// TierDecision is the tier a pull request falls in and why. Skip means no
// model is called; Agents, when set, are the only agents that review.
type TierDecision struct {
	Tier   string   `json:"tier"`
	Reason string   `json:"reason"`
	Skip   bool     `json:"skip,omitempty"`
	Agents []string `json:"agents,omitempty"`
}

func (tiers *Tiers) validate(agents []string) error {
	if tiers == nil {
		return nil
	}
	if err := validateGlobs(tiers.SensitivePaths); err != nil {
		return fmt.Errorf("tiers.sensitive_paths: %w", err)
	}
	for name, tier := range map[string]*Tier{TierTrivial: tiers.Trivial, TierLite: tiers.Lite} {
		if tier == nil {
			continue
		}
		if tier.MaxLines < 0 || tier.MaxLines > 100000 || tier.MaxFiles < 1 || tier.MaxFiles > MaxPreviewFiles {
			return fmt.Errorf("tiers.%s: max_lines must be 0 or more and max_files at least 1", name)
		}
		switch {
		case tier.Action != "" && (name != TierTrivial || tier.Action != TierSkip):
			return fmt.Errorf("tiers.%s: action must be empty or, for the trivial tier, skip", name)
		case tier.Action == TierSkip && len(tier.Agents) > 0:
			return errors.New("tiers.trivial: a skipped tier has no agents")
		case tier.Action == "" && len(tier.Agents) == 0:
			return fmt.Errorf("tiers.%s: list the agents that review, or (trivial only) set action to skip", name)
		}
		if len(tier.Agents) > 0 && len(agents) > 0 {
			hasMain := false
			for _, agent := range tier.Agents {
				if agent == "main" || agent == agents[0] {
					hasMain = true
					continue
				}
				if !slices.Contains(agents, agent) {
					return fmt.Errorf("tiers.%s: %q is not an agent of this workflow", name, agent)
				}
			}
			if !hasMain {
				return fmt.Errorf("tiers.%s: the agents must include the main agent (\"main\")", name)
			}
		}
	}
	if tiers.Trivial != nil && tiers.Lite != nil && (tiers.Trivial.MaxLines > tiers.Lite.MaxLines || tiers.Trivial.MaxFiles > tiers.Lite.MaxFiles) {
		return errors.New("tiers: the trivial tier must be no larger than the lite tier")
	}
	return nil
}

func (tier *Tier) fits(lines, files int) bool {
	return tier != nil && lines <= tier.MaxLines && files <= tier.MaxFiles
}

// ChooseTier picks the tier for a pull request's changed files, after the
// workflow's skip paths. Without tiers every pull request is full.
func (workflow ResolvedWorkflow) ChooseTier(changes []FileChange) TierDecision {
	tiers := workflow.Tiers
	if tiers == nil {
		return TierDecision{Tier: TierFull, Reason: "no tiers configured"}
	}
	skip, _ := compileGlobs(workflow.SkipPaths)
	sensitive, _ := compileGlobs(tiers.SensitivePaths)
	lines, files := 0, 0
	for _, change := range changes {
		if skip.match(change.Path) {
			continue
		}
		if sensitive.match(change.Path) {
			return TierDecision{Tier: TierFull, Reason: "touches a sensitive path: " + change.Path}
		}
		lines += change.Lines
		files++
	}
	size := fmt.Sprintf("%s in %s", Plural(lines, "changed line"), Plural(files, "file"))
	switch {
	case tiers.Trivial.fits(lines, files):
		decision := TierDecision{Tier: TierTrivial, Reason: size, Skip: tiers.Trivial.Action == TierSkip}
		if !decision.Skip {
			decision.Agents = workflow.tierAgents(tiers.Trivial.Agents)
		}
		return decision
	case tiers.Lite.fits(lines, files):
		return TierDecision{Tier: TierLite, Reason: size, Agents: workflow.tierAgents(tiers.Lite.Agents)}
	}
	return TierDecision{Tier: TierFull, Reason: size}
}

// tierAgents resolves "main" to the main agent's name.
func (workflow ResolvedWorkflow) tierAgents(names []string) []string {
	resolved := make([]string, 0, len(names))
	for _, name := range names {
		if name == "main" && len(workflow.Agents) > 0 {
			name = workflow.Agents[0].Name
		}
		if !slices.Contains(resolved, name) {
			resolved = append(resolved, name)
		}
	}
	return resolved
}

// WithAgents returns the workflow reviewing with only the named agents, in
// workflow order. The main agent always stays.
func (workflow ResolvedWorkflow) WithAgents(names []string) ResolvedWorkflow {
	if len(names) == 0 {
		return workflow
	}
	narrowed := workflow
	narrowed.Agents = nil
	for index, agent := range workflow.Agents {
		if index == 0 || slices.Contains(names, agent.Name) {
			narrowed.Agents = append(narrowed.Agents, agent)
		}
	}
	if !narrowed.HasPlannableAgents() {
		narrowed.PlannerPrompt = nil
	}
	return narrowed
}

// DiffChanges counts the changed lines of each file in a unified diff.
// Deleted files count under their old path.
func DiffChanges(patch string) []FileChange {
	var changes []FileChange
	current := -1
	inHunk := false
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			changes = append(changes, FileChange{})
			current, inHunk = len(changes)-1, false
		case current < 0:
		case !inHunk && strings.HasPrefix(line, "--- a/"):
			changes[current].Path = strings.TrimSuffix(strings.TrimPrefix(line, "--- a/"), "\t")
		case !inHunk && strings.HasPrefix(line, "+++ b/"):
			changes[current].Path = strings.TrimSuffix(strings.TrimPrefix(line, "+++ b/"), "\t")
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")):
			changes[current].Lines++
		}
	}
	return slices.DeleteFunc(changes, func(change FileChange) bool { return change.Path == "" })
}
