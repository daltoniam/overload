package diff

import (
	"strings"
	"testing"
)

func TestParseCommentableLines(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,4 @@\n first\n-old\n+new\n+more\n last\n"
	files, err := Parse(patch)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		line int
		want bool
	}{{1, false}, {2, true}, {3, true}, {4, false}} {
		if got := Commentable(files, "a.go", test.line); got != test.want {
			t.Errorf("line %d: got %v, want %v", test.line, got, test.want)
		}
	}
}

func TestParseGitHeaderForms(t *testing.T) {
	patch := "diff --git \"a/caf\\303\\251.md\" \"b/caf\\303\\251.md\"\nnew file mode 100644\n--- /dev/null\n+++ \"b/caf\\303\\251.md\"\n@@ -0,0 +1 @@\n+c\n" +
		"diff --git a/img.png b/img.png\nnew file mode 100644\nBinary files /dev/null and b/img.png differ\n" +
		"diff --git a/my file.go b/my file.go\nnew file mode 100644\n--- /dev/null\n+++ b/my file.go\t\n@@ -0,0 +1,2 @@\n+b\n+++ b/not-a-header.go\n" +
		"diff --git a/crlf.go b/crlf.go\r\n--- a/crlf.go\r\n+++ b/crlf.go\r\n@@ -1 +1 @@\r\n-a\r\n+b\r\n" +
		"diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n"
	files, err := Parse(patch)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	if len(paths) != 3 || paths[0] != "café.md" || paths[1] != "my file.go" || paths[2] != "crlf.go" {
		t.Fatalf("paths %q", paths)
	}
	if !Commentable(files, "my file.go", 2) || files[1].Lines[2] != "++ b/not-a-header.go" {
		t.Fatalf("an added line starting with ++ was read as a header: %+v", files[1].Lines)
	}
	if files[2].Lines[1] != "b" {
		t.Fatalf("CRLF line kept its carriage return: %q", files[2].Lines[1])
	}
}

func TestChangedPaths(t *testing.T) {
	for _, test := range []struct {
		name, patch string
		want        []string
		wantErr     bool
	}{
		{"modified", "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n", []string{"a.go"}, false},
		{"deleted", "diff --git a/auth/old.go b/auth/old.go\ndeleted file mode 100644\n--- a/auth/old.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n", []string{"auth/old.go"}, false},
		{"binary", "diff --git a/img.png b/img.png\nnew file mode 100644\nBinary files /dev/null and b/img.png differ\n", []string{"img.png"}, false},
		{"empty new file", "diff --git a/my file.go b/my file.go\nnew file mode 100644\n", []string{"my file.go"}, false},
		{"mode only", "diff --git a/run.sh b/run.sh\nold mode 100644\nnew mode 100755\n", []string{"run.sh"}, false},
		{"pure rename", "diff --git a/auth/key.go b/pkg/key.go\nsimilarity index 100%\nrename from auth/key.go\nrename to pkg/key.go\n", []string{"auth/key.go", "pkg/key.go"}, false},
		{"rename with spaces", "diff --git a/old name.go b/new name.go\nsimilarity index 100%\nrename from old name.go\nrename to new name.go\n", []string{"old name.go", "new name.go"}, false},
		{"copy", "diff --git a/a.go b/b.go\nsimilarity index 100%\ncopy from a.go\ncopy to b.go\n", []string{"a.go", "b.go"}, false},
		{"quoted", "diff --git \"a/caf\\303\\251.md\" \"b/caf\\303\\251.md\"\nnew file mode 100644\n", []string{"café.md"}, false},
		{"quoted old name, unquoted new name ending in a space", "diff --git \"a/old\\tname\" b/new \nsimilarity index 100%\nrename from \"old\\tname\"\nrename to new \n", []string{"old\tname", "new "}, false},
		{"hunk lines are not headers", "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,2 @@\n--- a/evil.go\n+++ b/evil.go\n", []string{"a.go"}, false},
		{"crlf", "diff --git a/crlf.go b/crlf.go\r\n--- a/crlf.go\r\n+++ b/crlf.go\r\n@@ -1 +1 @@\r\n-a\r\n+b\r\n", []string{"crlf.go"}, false},
		{"repeats once", "diff --git a/a.go b/a.go\nold mode 100644\nnew mode 100755\ndiff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n", []string{"a.go"}, false},
		{"unreadable section", "diff --git a/x b/y\nold mode 100644\nnew mode 100755\n", nil, true},
		{"quoted names without prefixes", "diff --git \"tab\\tname\" \"tab\\tname\"\n--- \"tab\\tname\"\n+++ \"tab\\tname\"\n@@ -1 +1 @@\n-x\n+y\n", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ChangedPaths(test.patch)
			if (err != nil) != test.wantErr || strings.Join(got, "|") != strings.Join(test.want, "|") {
				t.Fatalf("ChangedPaths = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}
