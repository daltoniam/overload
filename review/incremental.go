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
// included), each hunk's section heading, its source path, file modes
// (from mode lines or the index line) and rename or copy source, and for binary files the new content's blob ID.
// Hunk positions and other index lines are left out, so the same change
// hashes the same after a rebase or a merge from the base branch.
func FilePatchHashes(patch string) map[string]string {
	hashes := map[string]string{}
	var name string
	var body strings.Builder
	inHunk := false
	// Git can emit several sections for one path (a file that becomes a
	// symlink is a deletion plus an addition); they are hashed together.
	bodies := map[string]*strings.Builder{}
	var order []string
	flush := func() {
		if name != "" {
			combined, ok := bodies[name]
			if !ok {
				combined = &strings.Builder{}
				bodies[name] = combined
				order = append(order, name)
			}
			combined.WriteString(body.String())
			combined.WriteString("\x00")
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
			oldPath, newPath := overload.GitHeaderPaths(strings.TrimPrefix(line, "diff --git "))
			if name = newPath; name == "" {
				name = oldPath
			}
			// The source matters for renames and copies, which may have no
			// hunks: renaming a different file onto the same path is a
			// different change.
			body.WriteString("from " + oldPath + "\n")
		case !inHunk && (strings.HasPrefix(line, "new file mode ") || strings.HasPrefix(line, "deleted file mode ") || strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode ") ||
			strings.HasPrefix(line, "rename from ") || strings.HasPrefix(line, "copy from ")):
			body.WriteString(strings.TrimSuffix(line, "\r"))
			body.WriteByte('\n')
		case !inHunk && strings.HasPrefix(line, "index "):
			fields := strings.Fields(line)
			if len(fields) > 1 {
				if _, blob, ok := strings.Cut(fields[1], ".."); ok {
					newBlob = blob
				}
			}
			// The mode on an index line (a file whose mode did not change)
			// tells a regular file from a symlink; blob IDs are left out.
			if len(fields) > 2 {
				body.WriteString("mode " + fields[2] + "\n")
			}
		case !inHunk && (strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch"):
			body.WriteString("binary " + newBlob + "\n")
		case !inHunk && (strings.HasPrefix(line, "rename to ") || strings.HasPrefix(line, "copy to ")):
			_, to, _ := strings.Cut(line, " to ")
			if path, ok := overload.DiffPath(strings.TrimSuffix(to, "\r"), ""); ok {
				name = path
			}
		case !inHunk && strings.HasPrefix(line, "+++ "):
			if path, ok := overload.DiffPath(strings.TrimSuffix(strings.TrimPrefix(line, "+++ "), "\r"), "b/"); ok {
				name = path
			}
		case strings.HasPrefix(line, "@@"):
			inHunk = true
			// Keep the section text after the coordinates (the enclosing
			// function), not the line numbers.
			section := ""
			if end := strings.Index(line[2:], "@@"); end >= 0 {
				section = line[2+end+2:]
			}
			body.WriteString("@@" + section + "\n")
		case inHunk && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, `\`)):
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	flush()
	for _, path := range order {
		sum := sha256.Sum256([]byte(bodies[path].String()))
		hashes[path] = hex.EncodeToString(sum[:])
	}
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
	return &overload.Incremental{SinceRunID: rereview.SinceRunID, Unchanged: UnchangedFiles(rereview.PreviousDiff, diff), Reverted: RevertedFiles(rereview.PreviousDiff, diff), Prior: rereview.Prior}
}

// RevertedFiles lists the files the previous diff changes and the current
// one does not.
func RevertedFiles(previous, current string) []string {
	before, after := FilePatchHashes(previous), FilePatchHashes(current)
	var reverted []string
	for name := range before {
		if _, ok := after[name]; !ok {
			reverted = append(reverted, name)
		}
	}
	slices.Sort(reverted)
	return reverted
}
