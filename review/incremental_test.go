package review

import (
	"slices"
	"testing"
)

func TestUnchangedFilesSurviveRebases(t *testing.T) {
	previous := "diff --git a/a.go b/a.go\nindex 111..222 100644\n--- a/a.go\n+++ b/a.go\n@@ -10,2 +10,2 @@ func A() {\n-\told()\n+\tnew()\n }\ndiff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-x := 1\n+x := 2\n"
	// a.go: same change after a rebase moved it and changed the index line;
	// b.go: a further change; c.go: new.
	current := "diff --git a/a.go b/a.go\nindex 333..444 100644\n--- a/a.go\n+++ b/a.go\n@@ -40,2 +40,2 @@ func A() {\n-\told()\n+\tnew()\n }\ndiff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-x := 1\n+x := 3\ndiff --git a/c.go b/c.go\nnew file mode 100644\n--- /dev/null\n+++ b/c.go\n@@ -0,0 +1 @@\n+package c\n"
	if got := UnchangedFiles(previous, current); !slices.Equal(got, []string{"a.go"}) {
		t.Fatalf("unchanged = %v", got)
	}
	deleted := "diff --git a/d.go b/d.go\ndeleted file mode 100644\n--- a/d.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-package d\n"
	if hashes := FilePatchHashes(deleted); hashes["d.go"] == "" {
		t.Fatalf("deleted file not hashed: %v", hashes)
	}
}

func TestFinalNewlineChangesTheHash(t *testing.T) {
	with := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n"
	without := with + "\\ No newline at end of file\n"
	if len(UnchangedFiles(with, without)) != 0 {
		t.Fatal("a final-newline change counted as unchanged")
	}
}
