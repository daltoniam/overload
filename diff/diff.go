package diff

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var hunkPattern = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

type File struct {
	Path  string
	Lines map[int]string
}

func Parse(patch string) ([]File, error) {
	var files []File
	var current *File
	lineNumber := 0
	inHunk := false
	scanner := bufio.NewScanner(strings.NewReader(patch))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			files = append(files, File{Path: strings.TrimPrefix(line, "+++ b/"), Lines: make(map[int]string)})
			current = &files[len(files)-1]
			inHunk = false
		case strings.HasPrefix(line, "+++ /dev/null"):
			current = nil
			inHunk = false
		case strings.HasPrefix(line, "@@ "):
			matches := hunkPattern.FindStringSubmatch(line)
			if matches == nil {
				return nil, fmt.Errorf("invalid diff hunk: %q", line)
			}
			var err error
			lineNumber, err = strconv.Atoi(matches[1])
			if err != nil {
				return nil, err
			}
			inHunk = true
		case inHunk && current != nil && strings.HasPrefix(line, "+"):
			current.Lines[lineNumber] = line[1:]
			lineNumber++
		case inHunk && strings.HasPrefix(line, " "):
			lineNumber++
		case inHunk && strings.HasPrefix(line, "-"):
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
		}
	}
	return files, scanner.Err()
}

func Commentable(files []File, path string, line int) bool {
	for _, file := range files {
		if file.Path == path {
			_, ok := file.Lines[line]
			return ok
		}
	}
	return false
}
