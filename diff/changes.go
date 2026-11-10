package diff

import (
	"fmt"
	"strconv"
	"strings"
)

// ChangedPaths lists every file name a git diff touches, in diff order and
// without repeats: the new path of added and modified files, the old path
// of deleted files, both names of renames and copies, and binary, empty and
// mode-only files, which have no lines to comment on. A merge gate must see
// all of them, so a file section whose name cannot be read is an error.
func ChangedPaths(patch string) ([]string, error) {
	seen := map[string]bool{}
	var paths []string
	add := func(path string) {
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	section, inHeader, found := 0, false, false
	finishSection := func() error {
		if section > 0 && !found {
			return fmt.Errorf("diff section %d names no file", section)
		}
		return nil
	}
	for line := range strings.Lines(patch) {
		line = strings.TrimRight(line, "\r\n")
		if rest, ok := strings.CutPrefix(line, "diff --git "); ok {
			if err := finishSection(); err != nil {
				return nil, err
			}
			section, inHeader, found = section+1, true, false
			for _, path := range headerPaths(rest) {
				if path != "" {
					add(path)
					found = true
				}
			}
			continue
		}
		if !inHeader {
			continue
		}
		var path string
		switch {
		case strings.HasPrefix(line, "@@ "), strings.HasPrefix(line, "Binary files "), line == "GIT binary patch":
			inHeader = false
			continue
		case strings.HasPrefix(line, "--- "):
			path = prefixedPath(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ "):
			path = prefixedPath(strings.TrimPrefix(line, "+++ "), "b/")
		default:
			for _, prefix := range []string{"rename from ", "rename to ", "copy from ", "copy to "} {
				if rest, ok := strings.CutPrefix(line, prefix); ok {
					path = unquote(rest)
				}
			}
		}
		if path != "" {
			add(path)
			found = true
		}
	}
	if err := finishSection(); err != nil {
		return nil, err
	}
	return paths, nil
}

// headerPaths reads the names in a "diff --git a/old b/new" header. Git
// quotes names with unusual characters; unquoted names may contain spaces,
// so they are read only when both sides name the same file, and renames
// rely on their "rename from" and "rename to" lines instead.
func headerPaths(header string) []string {
	if strings.HasPrefix(header, `"`) {
		first, err := strconv.QuotedPrefix(header)
		if err != nil {
			return nil
		}
		paths := []string{prefixedPath(first, "a/")}
		// Only the separating space is removed: an unquoted second name may
		// itself end in spaces.
		if second, ok := strings.CutPrefix(header[len(first):], " "); ok && second != "" {
			paths = append(paths, prefixedPath(second, "b/"))
		}
		return paths
	}
	if (len(header)-5)%2 != 0 || len(header) < 7 {
		return nil
	}
	name := header[2 : 2+(len(header)-5)/2]
	if header != "a/"+name+" b/"+name {
		return nil
	}
	return []string{name}
}

// prefixedPath reads a possibly quoted name and removes git's "a/" or "b/"
// prefix; "/dev/null" names no file.
func prefixedPath(name, prefix string) string {
	name = unquote(strings.TrimSuffix(name, "\t"))
	path, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return ""
	}
	return path
}

func unquote(name string) string {
	if strings.HasPrefix(name, `"`) {
		if unquoted, err := strconv.Unquote(name); err == nil {
			return unquoted
		}
		return ""
	}
	return name
}
