package diff

import "testing"

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
