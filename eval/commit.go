package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxCommitFiles bounds the files a case built from a commit includes.
const MaxCommitFiles = 20

// CommitBug is the bug name every expected finding of a case built from a
// commit shares, so a fix with several hunks counts as one bug.
const CommitBug = "fix"

var (
	commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	hunkHeader    = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)
)

// stableDiff pins every diff option a user's git configuration could
// change (colour, external diff and textconv drivers, path prefixes,
// relative paths, rename detection), so a case is the same everywhere.
var stableDiff = []string{"--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-relative", "--src-prefix=a/", "--dst-prefix=b/"}

// CaseFromCommit builds a case from a commit that fixed a bug in the
// repository at dir: the reviewer sees the change that undoes the fix
// (from the fixed files back to the buggy parent), and the expected
// findings are the lines the fix changed, grouped as one bug. Only text
// files the commit modified are included; added, deleted, renamed and
// binary files are skipped.
func CaseFromCommit(ctx context.Context, dir, commit, name string) (Case, error) {
	if !commitPattern.MatchString(commit) {
		return Case{}, errors.New("give the fixing commit as a hash")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	git := func(args ...string) (string, error) {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.quotepath=off"}, args...)...)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		out, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return string(out), nil
	}
	diff := func(args ...string) (string, error) {
		return git(append(append([]string{"diff"}, stableDiff...), args...)...)
	}
	parents, err := git("rev-list", "--parents", "-n", "1", commit+"^{commit}", "--")
	if err != nil {
		return Case{}, err
	}
	ids := strings.Fields(parents)
	switch {
	case len(ids) < 2:
		return Case{}, errors.New("the commit has no parent to compare with")
	case len(ids) > 2:
		return Case{}, errors.New("the commit is a merge; pick the commit on its branch that fixed the bug")
	}
	commit, parent := ids[0], ids[1]
	subject, err := git("log", "-1", "--no-show-signature", "--format=%s", commit, "--")
	if err != nil {
		return Case{}, err
	}
	listing, err := diff("--numstat", "-z", "--diff-filter=M", parent, commit)
	if err != nil {
		return Case{}, err
	}
	var paths []string
	for _, record := range strings.Split(listing, "\x00") {
		fields := strings.SplitN(record, "\t", 3)
		// Binary files have no line counts.
		if len(fields) == 3 && fields[0] != "-" && fields[2] != "" {
			paths = append(paths, fields[2])
		}
	}
	if len(paths) == 0 {
		return Case{}, errors.New("the commit modifies no existing text files")
	}
	if len(paths) > MaxCommitFiles {
		return Case{}, fmt.Errorf("the commit modifies %d files; pick one that changes at most %d", len(paths), MaxCommitFiles)
	}
	total := 0
	for _, path := range paths {
		size, err := git("cat-file", "-s", parent+":"+path)
		if err != nil {
			return Case{}, err
		}
		fileBytes, err := strconv.Atoi(strings.TrimSpace(size))
		if err != nil {
			return Case{}, fmt.Errorf("size of %s: %w", path, err)
		}
		if total += fileBytes; total > MaxCaseBytes {
			return Case{}, fmt.Errorf("the commit's files are larger than %d KiB before the fix; pick a smaller fix", MaxCaseBytes>>10)
		}
	}
	if name == "" {
		name = "commit-" + commit[:12]
	}
	item := Case{Name: name, Description: strings.TrimSpace(subject)}
	var pathspecs []string
	for _, path := range paths {
		content, err := git("cat-file", "blob", parent+":"+path)
		if err != nil {
			return Case{}, err
		}
		if strings.ContainsRune(content, 0) {
			continue
		}
		// Paths are relative to the top of the repository, even when dir
		// is a subdirectory, and may contain glob characters.
		pathspec := ":(top,literal)" + path
		pathspecs = append(pathspecs, pathspec)
		item.Files = append(item.Files, CaseFile{Path: path, Content: content})
		fix, err := diff("-U0", parent, commit, "--", pathspec)
		if err != nil {
			return Case{}, err
		}
		for _, line := range strings.Split(fix, "\n") {
			match := hunkHeader.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			start, _ := strconv.Atoi(match[1])
			count := 1
			if match[2] != "" {
				count, _ = strconv.Atoi(match[2])
			}
			end := start + count - 1
			if count == 0 {
				// The fix only added lines after start (0 for the top of
				// the file); the bug is the code around that point.
				start = max(start, 1)
				end = start + 1
			}
			item.Expected = append(item.Expected, ExpectedFinding{Path: path, StartLine: start, EndLine: end, Description: item.Description, Bug: CommitBug})
		}
	}
	if len(pathspecs) == 0 {
		return Case{}, errors.New("the commit modifies no existing text files")
	}
	shown, err := diff(append([]string{"-U3", commit, parent, "--"}, pathspecs...)...)
	if err != nil {
		return Case{}, err
	}
	item.Diff = shown
	return item, item.Validate()
}
