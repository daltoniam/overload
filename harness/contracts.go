package harness

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
)

// symbolDefinitions find the names defined on one line, by language.
// They are deliberately simple: the index is a hint for the model, and a
// missed or extra symbol costs a few lines of context, not correctness.
var symbolDefinitions = map[string][]*regexp.Regexp{
	"go": {
		regexp.MustCompile(`^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*[\[(]`),
		regexp.MustCompile(`^\s*type\s+([A-Za-z_]\w*)\s`),
		regexp.MustCompile(`^\s*(?:const|var)\s+([A-Z]\w*)\b`),
	},
	"python": {
		regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`),
		regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)\b`),
		regexp.MustCompile(`^([A-Z][A-Z0-9_]+)\s*=`),
	},
	"script": {
		regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)\s*[<(]`),
		regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`),
		regexp.MustCompile(`^\s*export\s+(?:declare\s+)?(?:const|let|var|interface|type|enum)\s+([A-Za-z_$][\w$]*)`),
		regexp.MustCompile(`^\s*(?:export\s+)?const\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`),
		regexp.MustCompile(`^\s+(?:(?:public|private|protected|static|async|readonly|override)\s+)*([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*(?::\s*[^{=]+)?\{\s*$`),
	},
	"graphql": {
		regexp.MustCompile(`^\s*(?:extend\s+)?(?:type|interface|input|enum|union|scalar)\s+([A-Za-z_]\w*)`),
		regexp.MustCompile(`^\s+([A-Za-z_]\w*)\s*(?:\([^)]*\))?\s*:\s*[\[A-Za-z_!]`),
	},
}

// languageOf names the definition rules for a path, or "" for none.
func languageOf(name string) string {
	switch path.Ext(name) {
	case ".go":
		return "go"
	case ".py", ".pyi":
		return "python"
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts":
		return "script"
	case ".graphql", ".gql", ".graphqls":
		return "graphql"
	}
	return ""
}

// searchable lists the file extensions searched for uses.
var searchable = map[string]bool{".go": true, ".py": true, ".pyi": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true, ".mts": true, ".cts": true, ".graphql": true, ".gql": true, ".graphqls": true, ".vue": true, ".svelte": true, ".rb": true, ".java": true, ".kt": true, ".rs": true, ".swift": true, ".php": true, ".cs": true, ".sql": true, ".yaml": true, ".yml": true, ".json": true, ".proto": true}

// skippedDirs are not searched: dependencies, build output and VCS data.
var skippedDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, ".next": true, "__pycache__": true, ".venv": true, "venv": true, "target": true, ".mypy_cache": true, ".pytest_cache": true, "coverage": true}

// commonNames are too generic to search for: they would list unrelated
// uses and crowd out the ones that matter.
var commonNames = map[string]bool{"init": true, "main": true, "test": true, "setup": true, "setUp": true, "tearDown": true, "String": true, "Error": true, "Close": true, "Name": true, "Type": true, "value": true, "data": true, "name": true, "type": true, "constructor": true, "render": true, "default": true, "index": true, "Query": true, "Mutation": true, "Subscription": true, "__init__": true, "run": true, "Run": true, "get": true, "set": true, "New": true, "id": true, "ID": true}

// skipWords are control-flow words the method rule would otherwise read as
// definitions.
var skipWords = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true, "function": true, "return": true, "else": true, "with": true}

// Limits on the search for uses.
const (
	maxSearchFileBytes = 1 << 20
	maxUseText         = 160
)

// Budgets for one search for uses, variables so tests can lower them.
var (
	maxSearchFiles = 60000
	maxSearchBytes = 600 << 20
)

type symbolUse struct {
	Path   string
	Line   int
	Text   string
	InDiff bool
}

// contractChange is a symbol the pull request removes (or renames) or
// whose definition line changes, with where the head still uses it.
type contractChange struct {
	Symbol string
	Kind   string // "removed" or "changed"
	Path   string
	Before string
	After  string
	Uses   []symbolUse
	More   int
}

// contractsIndex is the deterministic cross-file part of the pull request
// bundle.
type contractsIndex struct {
	Changes   []contractChange
	Truncated bool
}

type definition struct {
	name, line, path string
}

// definitionsIn returns the symbols defined on the removed (-) or added
// (+) lines of one file's patch.
func definitionsIn(patch, filePath, sign string) []definition {
	rules := symbolDefinitions[languageOf(filePath)]
	if len(rules) == 0 {
		return nil
	}
	var found []definition
	for _, line := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(line, sign) || strings.HasPrefix(line, sign+sign+sign) {
			continue
		}
		text := line[1:]
		for _, rule := range rules {
			match := rule.FindStringSubmatch(text)
			if match == nil || skipWords[match[1]] {
				continue
			}
			found = append(found, definition{name: match[1], line: strings.TrimSpace(text), path: filePath})
			break
		}
	}
	return found
}

// patchPaths returns the old and new path of one file's patch section.
func oldPath(patch string) string {
	for _, line := range strings.Split(patch, "\n") {
		if name, ok := strings.CutPrefix(line, "--- a/"); ok {
			return strings.TrimSuffix(name, "\t")
		}
		if strings.HasPrefix(line, "@@") {
			break
		}
	}
	return ""
}

// buildContractsIndex finds the symbols the diff removes or changes and
// searches the head tree for their remaining uses. It reads only repo.
func buildContractsIndex(ctx context.Context, parsed reviewDiff, repo fs.FS, maxCallers int) contractsIndex {
	inDiff := map[string]bool{}
	added := map[string][]definition{}
	var removed []definition
	for _, section := range parsed.sections {
		name := section.path
		if name == "" {
			name = oldPath(section.patch)
		}
		if name == "" {
			continue
		}
		inDiff[name] = true
		for _, def := range definitionsIn(section.patch, name, "+") {
			added[def.name] = append(added[def.name], def)
		}
		removed = append(removed, definitionsIn(section.patch, name, "-")...)
	}
	var index contractsIndex
	// A definition is identified by its file and name: two modules may
	// each change their own validate.
	type scoped struct{ path, name string }
	seen := map[scoped]bool{}
	for _, def := range removed {
		key := scoped{def.path, def.name}
		if seen[key] || len(def.name) < 3 || commonNames[def.name] {
			continue
		}
		seen[key] = true
		change := contractChange{Symbol: def.name, Kind: "removed", Path: def.path, Before: truncateLine(def.line)}
		// The new definition is looked for in the same file first; only a
		// definition that left its file is matched elsewhere (a move).
		again := slices.DeleteFunc(slices.Clone(added[def.name]), func(other definition) bool { return other.path != def.path })
		if len(again) == 0 {
			again = added[def.name]
		}
		if len(again) > 0 {
			if slices.ContainsFunc(again, func(other definition) bool { return other.line == def.line }) {
				continue
			}
			change.Kind, change.After = "changed", truncateLine(again[0].line)
		}
		index.Changes = append(index.Changes, change)
	}
	if len(index.Changes) == 0 || repo == nil {
		return index
	}
	var names []string
	// position lists the changes of each symbol; a use of a name several
	// files define counts for each of them.
	position := map[string][]int{}
	for i, change := range index.Changes {
		if _, ok := position[change.Symbol]; !ok {
			names = append(names, regexp.QuoteMeta(change.Symbol))
		}
		position[change.Symbol] = append(position[change.Symbol], i)
	}
	pattern := regexp.MustCompile(`(?:` + strings.Join(names, "|") + `)`)
	// Only the first maxCallers uses of each change, in report order, are
	// kept while scanning; the rest are counted. A file with thousands of
	// uses (minified code) cannot grow memory.
	kept := make([][]symbolUse, len(index.Changes))
	counts := make([]int, len(index.Changes))
	files, bytesRead := 0, 0
	_ = fs.WalkDir(repo, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return fs.SkipDir
		}
		if entry.IsDir() {
			if name != "." && skippedDirs[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !searchable[path.Ext(name)] || !entry.Type().IsRegular() {
			return nil
		}
		if files >= maxSearchFiles || bytesRead >= maxSearchBytes {
			index.Truncated = true
			return fs.SkipAll
		}
		info, err := entry.Info()
		if err != nil || info.Size() > maxSearchFileBytes {
			return nil
		}
		data, err := fs.ReadFile(repo, name)
		if err != nil {
			return nil
		}
		// Every file read counts against the budgets, binary ones too.
		files++
		bytesRead += len(data)
		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 || !pattern.Match(data) {
			return nil
		}
		for number, line := range strings.Split(string(data), "\n") {
			if number%1024 == 0 && ctx.Err() != nil {
				return fs.SkipAll
			}
			symbols := identifiersIn(line, position)
			if len(symbols) == 0 {
				continue
			}
			// The line's definitions are read once, however many uses it has.
			defined := definedNames(line, name)
			seen := map[int]bool{}
			for _, symbol := range symbols {
				if slices.Contains(defined, symbol) {
					continue
				}
				for _, i := range position[symbol] {
					// One use per change and line.
					if seen[i] {
						continue
					}
					seen[i] = true
					counts[i]++
					use := symbolUse{Path: name, Line: number + 1, InDiff: inDiff[name]}
					kept[i] = keepFirstUses(kept[i], use, maxCallers, line)
				}
			}
		}
		return nil
	})
	for i := range index.Changes {
		index.Changes[i].Uses = kept[i]
		if counts[i] > len(kept[i]) {
			index.Changes[i].More = counts[i] - len(kept[i])
		}
	}
	return index
}

// compareUses orders uses for the report: uses outside the diff first
// (the callers a per-file review cannot see), then by path and line.
func compareUses(a, b symbolUse) int {
	if a.InDiff != b.InDiff {
		if a.InDiff {
			return 1
		}
		return -1
	}
	if a.Path != b.Path {
		return strings.Compare(a.Path, b.Path)
	}
	return a.Line - b.Line
}

// keepFirstUses adds use to uses, sorted by compareUses, keeping at most
// limit. The use's text is made only when it is kept.
func keepFirstUses(uses []symbolUse, use symbolUse, limit int, line string) []symbolUse {
	if limit <= 0 {
		return uses
	}
	at, _ := slices.BinarySearchFunc(uses, use, compareUses)
	if at >= limit {
		return uses
	}
	use.Text = truncateUse(line)
	uses = slices.Insert(uses, at, use)
	if len(uses) > limit {
		uses = uses[:limit:limit]
	}
	return uses
}

// definedNames returns the symbols line defines, by any rule of its
// language. Each rule runs once per line.
func definedNames(line, filePath string) []string {
	var names []string
	for _, rule := range symbolDefinitions[languageOf(filePath)] {
		if match := rule.FindStringSubmatch(line); match != nil {
			names = append(names, match[1])
		}
	}
	return names
}

func truncateUse(line string) string {
	line = strings.TrimSpace(line)
	if len(line) > maxUseText {
		line = line[:maxUseText] + "..."
	}
	return strings.ToValidUTF8(line, "")
}

// render writes the index within limit characters, dropping whole
// entries from the end when it does not fit.
func (index contractsIndex) render(limit int) string {
	if len(index.Changes) == 0 {
		return ""
	}
	const header = "Contracts index: symbols this pull request removes or changes, and where the head still uses them. A use outside the diff that still expects the old form is likely a bug.\n"
	const truncatedNote = "(the search for uses stopped early in a very large repository)\n"
	omittedNote := func(count int) string { return fmt.Sprintf("(%d more changed symbols left out to fit)\n", count) }
	// Room for the header and the longest footers is set aside before any
	// entry is admitted, so the result never exceeds limit.
	overhead := len(header) + len(omittedNote(len(index.Changes)))
	if index.Truncated {
		overhead += len(truncatedNote)
	}
	if overhead >= limit {
		return ""
	}
	var out strings.Builder
	out.WriteString(header)
	used, written, omitted := overhead, 0, 0
	for _, change := range index.Changes {
		var entry strings.Builder
		fmt.Fprintf(&entry, "- %s %s in %s\n  before: %s\n", change.Symbol, change.Kind, change.Path, change.Before)
		if change.After != "" {
			fmt.Fprintf(&entry, "  after: %s\n", change.After)
		}
		if len(change.Uses) == 0 {
			entry.WriteString("  no remaining uses found\n")
		}
		for _, use := range change.Uses {
			marker := ""
			if use.InDiff {
				marker = " (changed in this pull request)"
			}
			fmt.Fprintf(&entry, "  %s:%d%s: %s\n", use.Path, use.Line, marker, use.Text)
		}
		if change.More > 0 {
			fmt.Fprintf(&entry, "  and %d more uses\n", change.More)
		}
		if used+entry.Len() > limit {
			omitted++
			continue
		}
		used += entry.Len()
		written++
		out.WriteString(entry.String())
	}
	if written == 0 {
		return ""
	}
	if omitted > 0 {
		out.WriteString(omittedNote(omitted))
	}
	if index.Truncated {
		out.WriteString(truncatedNote)
	}
	return out.String()
}

// linked returns, for each changed file, the other files that use symbols
// it removes or changes.
func (index contractsIndex) linked() map[string][]string {
	links := map[string][]string{}
	for _, change := range index.Changes {
		for _, use := range change.Uses {
			if use.Path != change.Path && !slices.Contains(links[change.Path], use.Path) {
				links[change.Path] = append(links[change.Path], use.Path)
			}
		}
	}
	return links
}

func identifierByte(b byte) bool {
	return b == '_' || b == '$' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// identifiersIn returns the identifiers in line that are listed symbols,
// one per occurrence. Splitting the line into identifiers checks both
// boundaries of every name, so First(Second()) yields both.
func identifiersIn[V any](line string, symbols map[string]V) []string {
	var found []string
	for start := 0; start < len(line); {
		if !identifierByte(line[start]) {
			start++
			continue
		}
		end := start
		for end < len(line) && identifierByte(line[end]) {
			end++
		}
		if _, ok := symbols[line[start:end]]; ok {
			found = append(found, line[start:end])
		}
		start = end
	}
	return found
}
