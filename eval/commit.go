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

var (
	commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	hunkHeader    = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)
)

// CaseFromCommit builds a case from a commit that fixed a bug in the
// repository at dir: the reviewer sees the change that undoes the fix
// (from the fixed files back to the buggy parent), and the expected
// findings are the lines the fix changed. Only files the commit modified
// are included; added, deleted and renamed files are skipped.
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
	full, err := git("rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return Case{}, err
	}
	commit = strings.TrimSpace(full)
	subject, err := git("log", "-1", "--format=%s", commit)
	if err != nil {
		return Case{}, err
	}
	parent := commit + "^"
	listing, err := git("diff", "--name-only", "--diff-filter=M", "-z", parent, commit)
	if err != nil {
		return Case{}, err
	}
	var paths []string
	for _, path := range strings.Split(listing, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return Case{}, errors.New("the commit modifies no existing files")
	}
	if len(paths) > MaxCommitFiles {
		return Case{}, fmt.Errorf("the commit modifies %d files; pick one that changes at most %d", len(paths), MaxCommitFiles)
	}
	if name == "" {
		name = "commit-" + commit[:12]
	}
	item := Case{Name: name, Description: strings.TrimSpace(subject)}
	for _, path := range paths {
		content, err := git("show", parent+":"+path)
		if err != nil {
			return Case{}, err
		}
		item.Files = append(item.Files, CaseFile{Path: path, Content: content})
		fix, err := git("diff", "-U0", parent, commit, "--", path)
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
				// The fix only added lines after start; the bug is the code
				// around that point.
				start, end = max(start, 1), max(start, 1)+1
			}
			item.Expected = append(item.Expected, ExpectedFinding{Path: path, StartLine: max(start, 1), EndLine: end, Description: item.Description})
		}
	}
	diff, err := git(append([]string{"diff", "--no-color", "--no-ext-diff", commit, parent, "--"}, paths...)...)
	if err != nil {
		return Case{}, err
	}
	item.Diff = diff
	return item, item.Validate()
}
