package review

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
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

func TestPatchHashSeesModesLineEndingsAndBinaries(t *testing.T) {
	added := func(mode, line string) string {
		return "diff --git a/run.sh b/run.sh\nnew file mode " + mode + "\nindex 0000000..1111111\n--- /dev/null\n+++ b/run.sh\n@@ -0,0 +1 @@\n+" + line + "\n"
	}
	for name, pair := range map[string][2]string{
		"mode":        {added("100755", "#!/bin/sh"), added("100644", "#!/bin/sh")},
		"line ending": {added("100755", "#!/bin/sh"), added("100755", "#!/bin/sh\r")},
		"binary": {
			"diff --git a/logo.png b/logo.png\nindex 1111111..2222222 100644\nBinary files a/logo.png and b/logo.png differ\n",
			"diff --git a/logo.png b/logo.png\nindex 1111111..3333333 100644\nBinary files a/logo.png and b/logo.png differ\n",
		},
		"rename source": {
			"diff --git a/old-a b/target\nsimilarity index 100%\nrename from old-a\nrename to target\n",
			"diff --git a/old-b b/target\nsimilarity index 100%\nrename from old-b\nrename to target\n",
		},
		"rename or copy": {
			"diff --git a/old b/target\nsimilarity index 100%\nrename from old\nrename to target\n",
			"diff --git a/old b/target\nsimilarity index 100%\ncopy from old\ncopy to target\n",
		},
	} {
		if len(UnchangedFiles(pair[0], pair[1])) != 0 {
			t.Errorf("%s change counted as unchanged", name)
		}
	}
	inCharge := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -10,3 +10,3 @@ func Charge() {\n \tok := true\n-\treturn nil\n+\treturn err\n"
	inRefund := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -40,3 +40,3 @@ func Refund() {\n \tok := true\n-\treturn nil\n+\treturn err\n"
	if len(UnchangedFiles(inCharge, inRefund)) != 0 {
		t.Error("the same edit moved to another function counted as unchanged")
	}
	movedDown := strings.Replace(inCharge, "@@ -10,3 +10,3 @@", "@@ -25,3 +25,3 @@", 1)
	if len(UnchangedFiles(inCharge, movedDown)) != 1 {
		t.Error("the same edit in the same function at another line counted as changed")
	}
	rebased := "diff --git a/a.go b/a.go\nindex 9999999..8888888 100644\n--- a/a.go\n+++ b/a.go\n@@ -5 +5 @@\n-x\n+y\n"
	original := "diff --git a/a.go b/a.go\nindex 1111111..2222222 100644\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n"
	if len(UnchangedFiles(original, rebased)) != 1 {
		t.Fatal("a rebased text change counted as changed")
	}
	rename := "diff --git a/old-a b/target\nsimilarity index 100%\nrename from old-a\nrename to target\n"
	if len(UnchangedFiles(rename, rename)) != 1 {
		t.Fatal("the same rename counted as changed")
	}
}

func TestCacheWriteFailureDoesNotFailTheDownload(t *testing.T) {
	dir := t.TempDir()
	inner := &countingDownloads{fakePRSource: fakePRSource{archive: []byte("archive")}}
	source := CachedSource{PullRequestSource: inner, Cache: &ArchiveCache{Dir: dir}}
	sha := strings.Repeat("d", 40)
	// The cache directory cannot be written: the download still works.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var out bytes.Buffer
	if err := source.DownloadHead(context.Background(), "acme/api", sha, &out); err != nil || out.String() != "archive" {
		t.Fatalf("download = %q %v", out.String(), err)
	}
	copy := &bestEffort{file: os.NewFile(^uintptr(0), "closed")}
	if n, err := copy.Write([]byte("abc")); n != 3 || err != nil || copy.err == nil {
		t.Fatalf("best-effort write = %d %v %v", n, err, copy.err)
	}
}
