package review

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/daltoniam/overload"
)

// FilePatchHashes identifies each file's change in a diff by its changed
// and context lines (carriage returns and final-newline markers
// included), its file modes, and for binary files the new content's blob
// ID. Hunk positions and other index lines are left out, so the same
// change hashes the same after a rebase or a merge from the base branch.
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
	newBlob := ""
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			newBlob = ""
			// Files without --- and +++ lines (binary, mode-only) are named
			// by the header; the +++ line wins when present.
			if oldPath, newPath := overload.GitHeaderPaths(strings.TrimPrefix(line, "diff --git ")); newPath != "" {
				name = newPath
			} else {
				name = oldPath
			}
		case !inHunk && (strings.HasPrefix(line, "new file mode ") || strings.HasPrefix(line, "deleted file mode ") || strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode ")):
			body.WriteString(strings.TrimSuffix(line, "\r"))
			body.WriteByte('\n')
		case !inHunk && strings.HasPrefix(line, "index "):
			if fields := strings.Fields(line); len(fields) > 1 {
				if _, blob, ok := strings.Cut(fields[1], ".."); ok {
					newBlob = blob
				}
			}
		case !inHunk && (strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch"):
			body.WriteString("binary " + newBlob + "\n")
		case !inHunk && strings.HasPrefix(line, "+++ "):
			if path, ok := overload.DiffPath(strings.TrimSuffix(strings.TrimPrefix(line, "+++ "), "\r"), "b/"); ok {
				name = path
			}
		case strings.HasPrefix(line, "@@"):
			inHunk = true
			body.WriteString("@@\n")
		case inHunk && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, `\`)):
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
