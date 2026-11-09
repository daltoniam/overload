package review

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/daltoniam/overload"
)

// FilePatchHashes identifies each file's change in a diff by its changed
// and context lines alone. Hunk positions, index lines and commit IDs are
// left out, so the same change hashes the same after a rebase or a merge
// from the base branch.
func FilePatchHashes(patch string) map[string]string {
	hashes := map[string]string{}
	var name string
	var body strings.Builder
	inHunk := false
	flush := func() {
		if name != "" {
			sum := sha256.Sum256([]byte(body.String()))
			hashes[name] = hex.EncodeToString(sum[:])
		}
		name, inHunk = "", false
		body.Reset()
	}
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
		case !inHunk && strings.HasPrefix(line, "+++ "):
			if path, ok := strings.CutPrefix(strings.TrimSuffix(strings.TrimPrefix(line, "+++ "), "\t"), "b/"); ok {
				name = path
			}
		case !inHunk && strings.HasPrefix(line, "--- ") && name == "":
			if path, ok := strings.CutPrefix(strings.TrimSuffix(strings.TrimPrefix(line, "--- "), "\t"), "a/"); ok {
				name = path
			}
		case strings.HasPrefix(line, "@@"):
			inHunk = true
			body.WriteString("@@\n")
		case inHunk && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ")):
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	flush()
	return hashes
}

// UnchangedFiles lists the files whose change is the same in both diffs.
func UnchangedFiles(previous, current string) []string {
	before, after := FilePatchHashes(previous), FilePatchHashes(current)
	var unchanged []string
	for name, hash := range after {
		if before[name] == hash {
			unchanged = append(unchanged, name)
		}
	}
	slices.Sort(unchanged)
	return unchanged
}

// Rereview is what a re-review needs from the earlier one: its diff and
// the pull request's open findings.
type Rereview struct {
	SinceRunID   int64
	PreviousDiff string
	Prior        []overload.PriorFinding
}

// incremental compares diffs to describe the re-review.
func (rereview *Rereview) incremental(diff string) *overload.Incremental {
	if rereview == nil {
		return nil
	}
	return &overload.Incremental{SinceRunID: rereview.SinceRunID, Unchanged: UnchangedFiles(rereview.PreviousDiff, diff), Prior: rereview.Prior}
}
