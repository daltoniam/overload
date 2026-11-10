// Package checkout reviews and fixes changes in a local git checkout,
// without GitHub: the diff is the branch's changes against a base, and the
// repository is the working tree.
package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/harness"
)

var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@{}^~-]{0,199}$`)

// gitSafety turns off configuration that would run programs from the
// repository or the user's setup while overload drives git: hooks, file
// system monitors and signing prompts. A fixer worktree holds model-written
// files, so nothing in it may decide what git executes.
var gitSafety = []string{"-c", "core.quotepath=off", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}

// gitLocationVariables would point git at another repository; hooks and
// editors that start overload set them.
var gitLocationVariables = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM"}

// isInheritedGitConfig reports whether an environment variable carries git
// configuration from whoever started overload. It is dropped so the filter
// drivers Git lists are the ones git will see, and so Git's own
// GIT_CONFIG_COUNT settings are not mixed with someone else's.
func isInheritedGitConfig(name string) bool {
	return name == "GIT_CONFIG_PARAMETERS" || name == "GIT_CONFIG_COUNT" || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

func gitEnvironment() []string {
	return slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return slices.Contains(gitLocationVariables, name) || isInheritedGitConfig(name)
	})
}

// Git runs git in dir and returns its standard output. Every filter driver
// in the configuration is switched off first: a clean, smudge or process
// filter runs a program whenever git reads or writes a working-tree file
// whose attributes name it, and a branch under review or a fixer can add
// those attributes. Files are then read and written as they are stored, so
// a Git LFS file shows as its pointer.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	environment := gitEnvironment()
	drivers, err := runGit(ctx, dir, environment, append(globalOptions(args), "config", "-z", "--get-regexp", `^filter\.`)...)
	// Exit status 1 means no filter is configured.
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
		return "", fmt.Errorf("git config: %w", err)
	}
	names := filterDrivers(drivers)
	if len(names) > 0 {
		var settings []string
		for _, name := range names {
			for _, setting := range [][2]string{{"clean", ""}, {"smudge", ""}, {"process", ""}, {"required", "false"}} {
				settings = append(settings, "GIT_CONFIG_KEY_"+fmt.Sprint(len(settings)/2)+"=filter."+name+"."+setting[0], "GIT_CONFIG_VALUE_"+fmt.Sprint(len(settings)/2)+"="+setting[1])
			}
		}
		environment = append(append(environment, "GIT_CONFIG_COUNT="+fmt.Sprint(len(settings)/2)), settings...)
	}
	out, err := runGit(ctx, dir, environment, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", subcommand(args), err)
	}
	return out, nil
}

// filterDrivers lists the driver names in the output of git config -z
// --get-regexp '^filter\.': keys look like filter.<name>.<setting>, and a
// name may contain dots.
func filterDrivers(output string) []string {
	var names []string
	for entry := range strings.SplitSeq(output, "\x00") {
		key, _, _ := strings.Cut(entry, "\n")
		rest, ok := strings.CutPrefix(key, "filter.")
		if index := strings.LastIndex(rest, "."); ok && index > 0 && !slices.Contains(names, rest[:index]) {
			names = append(names, rest[:index])
		}
	}
	return names
}

// separateValueOptions are git's global options that may take their value
// as the next argument, as in --git-dir PATH; each also accepts
// --option=value. --super-prefix is gone from recent git but accepted a
// separate value in older releases.
var separateValueOptions = []string{"-c", "-C", "--git-dir", "--work-tree", "--namespace", "--config-env", "--attr-source", "--super-prefix"}

// globalOptions are the options before the subcommand, such as --git-dir,
// so listing filters reads the same configuration the command will.
func globalOptions(args []string) []string {
	for index := 0; index < len(args); index++ {
		switch {
		case slices.Contains(separateValueOptions, args[index]):
			index++
		case !strings.HasPrefix(args[index], "-"):
			return slices.Clone(args[:index])
		}
	}
	return slices.Clone(args)
}

func runGit(ctx context.Context, dir string, environment []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append(append([]string{"-C", dir}, gitSafety...), args...)...)
	command.Env = environment
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// subcommand names a git invocation in errors: the first argument that is
// not a global option.
func subcommand(args []string) string {
	if global := globalOptions(args); len(global) < len(args) {
		return args[len(global)]
	}
	return strings.Join(args, " ")
}

// repository is a checkout git commands run in. global holds options that
// pin the repository, so a fixer worktree's own .git file is never read.
type repository struct {
	dir    string
	global []string
}

func (repo repository) git(ctx context.Context, args ...string) (string, error) {
	return Git(ctx, repo.dir, append(slices.Clone(repo.global), args...)...)
}

// openRepository finds the top of the checkout containing dir, so diff
// paths and file reads agree when dir is a subdirectory.
func openRepository(ctx context.Context, dir string) (repository, error) {
	top, err := Git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return repository{}, err
	}
	return repository{dir: strings.TrimSpace(top)}, nil
}

// diffOptions keep the diff in the form the reviewer parses whatever the
// user's git configuration says: a/ and b/ prefixes, paths from the
// repository root, no external or text conversion programs, and submodule
// commit updates shown even when diff.ignoreSubmodules would hide them.
// Changes inside a submodule's working tree are left out: finding them
// runs git status in the submodule, which reads the submodule's own
// configuration and so its filters, which Git cannot switch off.
var diffOptions = []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", "--no-relative", "--submodule=short", "--ignore-submodules=dirty"}

// mergeBase is the commit where HEAD left base.
func (repo repository) mergeBase(ctx context.Context, base string) (string, error) {
	if !refPattern.MatchString(base) || strings.Contains(base, "..") {
		return "", errors.New("invalid base reference")
	}
	mergeBase, err := repo.git(ctx, "merge-base", base, "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(mergeBase), nil
}

// diff returns the changes since HEAD left base: committed and uncommitted
// changes to tracked files, or only committed ones.
func (repo repository) diff(ctx context.Context, base string, committedOnly bool) (string, error) {
	mergeBase, err := repo.mergeBase(ctx, base)
	if err != nil {
		return "", err
	}
	args := append(slices.Clone(diffOptions), mergeBase)
	if committedOnly {
		args = append(args, "HEAD")
	}
	diff, err := repo.git(ctx, append(args, "--")...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(diff) == "" {
		if committedOnly {
			return "", fmt.Errorf("no committed changes since %s", base)
		}
		return "", fmt.Errorf("no changes since %s", base)
	}
	return diff, nil
}

// Diff returns the changes in dir's checkout since it branched from base:
// committed and uncommitted changes to tracked files. Untracked files are
// left out.
func Diff(ctx context.Context, dir, base string) (string, error) {
	repo, err := openRepository(ctx, dir)
	if err != nil {
		return "", err
	}
	return repo.diff(ctx, base, false)
}

// Review reviews the changes since base in the checkout containing dir
// with workflow.
func Review(ctx context.Context, dir, base string, workflow overload.ResolvedWorkflow) (overload.ReviewResult, error) {
	repo, err := openRepository(ctx, dir)
	if err != nil {
		return overload.ReviewResult{}, err
	}
	return repo.review(ctx, base, workflow)
}

// review reads files through an os.Root so a symbolic link in the checkout
// cannot send a file from elsewhere on the machine (keys, credentials) to
// a hosted model.
func (repo repository) review(ctx context.Context, base string, workflow overload.ResolvedWorkflow) (overload.ReviewResult, error) {
	diff, err := repo.diff(ctx, base, false)
	if err != nil {
		return overload.ReviewResult{}, err
	}
	root, err := os.OpenRoot(repo.dir)
	if err != nil {
		return overload.ReviewResult{}, err
	}
	defer func() { _ = root.Close() }()
	return harness.Reviewer{}.Review(ctx, overload.ReviewSpec{Diff: diff, Workflow: workflow}, root.FS())
}

// Routing reads a result's routing.
func Routing(result overload.ReviewResult) overload.Routing {
	routing, _ := result.Metrics["routing"].(overload.Routing)
	return routing
}

// Gate is the merge gate a checkout review would set.
func Gate(workflow overload.ResolvedWorkflow, result overload.ReviewResult) overload.Gate {
	degraded, _ := result.Metrics["degraded"].(bool)
	severities := make([]string, 0, len(result.Findings))
	for _, finding := range result.Findings {
		severities = append(severities, finding.Severity)
	}
	return overload.GateOutcome(overload.GateInput{Decision: workflow.ReviewDecision, BlockSeverity: workflow.BlockSeverity, HumanReviewPaths: workflow.HumanReviewPaths, ApprovePaths: workflow.ApprovePaths, Routing: Routing(result), Degraded: degraded, Severities: severities})
}

// Report writes a review as Markdown.
func Report(w io.Writer, workflow overload.ResolvedWorkflow, result overload.ReviewResult) error {
	gate := Gate(workflow, result)
	routing := Routing(result)
	var out strings.Builder
	fmt.Fprintf(&out, "# Review: %s\n\n", gate.Description)
	if routing.Tier != "" {
		fmt.Fprintf(&out, "Risk tier: %s.\n", routing.Tier)
	}
	for _, agent := range routing.Agents {
		switch {
		case agent.OutOfTier:
			fmt.Fprintf(&out, "- %s: not needed for a %s change\n", agent.Agent, routing.Tier)
		case len(agent.Files) > 0:
			fmt.Fprintf(&out, "- %s reviewed %d of %s, %s\n", agent.Agent, agent.Reviewed, overload.Plural(len(agent.Files), "file"), overload.Plural(agent.Findings, "finding"))
		}
	}
	for _, note := range routing.Degraded {
		fmt.Fprintf(&out, "- Partial: %s\n", note)
	}
	if len(gate.HumanFiles) > 0 {
		fmt.Fprintf(&out, "- Needs a person: %s\n", strings.Join(gate.HumanFiles, ", "))
	}
	out.WriteString("\n")
	if len(result.Findings) == 0 {
		out.WriteString("No findings. This does not prove the change is bug-free.\n")
	}
	for _, finding := range result.Findings {
		fmt.Fprintf(&out, "## %s:%d [%s] %s\n\n%s\n\n", finding.Path, finding.Line, finding.Severity, finding.Title, finding.Body)
		if finding.Evidence != "" {
			fmt.Fprintf(&out, "```\n%s\n```\n\n", finding.Evidence)
		}
	}
	_, err := io.WriteString(w, out.String())
	return err
}
