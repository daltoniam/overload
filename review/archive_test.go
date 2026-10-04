package review

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractArchive(t *testing.T) {
	for _, test := range []struct {
		name      string
		entry     string
		entryType byte
		wantError bool
	}{
		{"regular", "prefix/file.go", tar.TypeReg, false},
		{"directory", "prefix/subdir/", tar.TypeDir, false},
		{"traversal", "prefix/../../escape", tar.TypeReg, true},
		{"symlink ignored", "prefix/shortcut", tar.TypeSymlink, false},
		{"hardlink ignored", "prefix/hardlink", tar.TypeLink, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			zip := gzip.NewWriter(&buffer)
			writer := tar.NewWriter(zip)
			content := []byte("hello")
			if test.entryType != tar.TypeReg {
				content = nil
			}
			if err := writer.WriteHeader(&tar.Header{Name: test.entry, Typeflag: test.entryType, Size: int64(len(content)), Linkname: "/etc/passwd"}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(content); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := zip.Close(); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			err := ExtractArchive(context.Background(), &buffer, dir)
			if (err != nil) != test.wantError {
				t.Fatalf("extract error: %v", err)
			}
			if (test.entryType == tar.TypeSymlink || test.entryType == tar.TypeLink) && !test.wantError {
				if _, statErr := os.Lstat(filepath.Join(dir, filepath.Base(test.entry))); !os.IsNotExist(statErr) {
					t.Fatalf("link should not be extracted: %v", statErr)
				}
			}
			if test.entryType == tar.TypeDir && !test.wantError {
				info, statErr := os.Stat(filepath.Join(dir, "subdir"))
				if statErr != nil || !info.IsDir() {
					t.Fatalf("directory missing: %v", statErr)
				}
			}
			if test.entryType == tar.TypeReg && !test.wantError {
				data, err := os.ReadFile(filepath.Join(dir, "file.go"))
				if err != nil || string(data) != "hello" {
					t.Fatalf("file=%q error=%v", data, err)
				}
			}
		})
	}
}
