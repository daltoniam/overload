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
