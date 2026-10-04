package github

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestTokenPRFetch(t *testing.T) {
	sha := strings.Repeat("b", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing GitHub auth")
		}
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/42":
			if strings.Contains(r.Header.Get("Accept"), "diff") {
				_, _ = w.Write([]byte("diff --git a/file b/file\n"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "open", "head": map[string]string{"sha": sha}, "base": map[string]string{"sha": strings.Repeat("a", 40)}})
		case "/repos/owner/repo/tarball/" + sha:
			_, _ = w.Write([]byte("archive"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewTokenClient("test-token")
	if err != nil {
		t.Fatal(err)
	}
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client.api.BaseURL = baseURL
	pr, diff, err := client.PullRequestWithDiff(context.Background(), "owner/repo", 42)
	if err != nil || pr.GetHead().GetSHA() != sha || !strings.Contains(diff, "diff --git") {
		t.Fatalf("pull request: %+v %q %v", pr, diff, err)
	}
	var archive bytes.Buffer
	if err := client.DownloadHead(context.Background(), "owner/repo", sha, &archive); err != nil || archive.String() != "archive" {
		t.Fatalf("archive: %q %v", archive.String(), err)
	}
}

func TestArchiveRedirectsStayOnGitHub(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example/archive.tar.gz?token=leaked-secret", http.StatusFound)
	}))
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/tarball?token=leaked-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = downloadArchive(context.Background(), request, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "attacker.example is not allowed") || strings.Contains(err.Error(), "leaked-secret") {
		t.Fatalf("redirect not blocked or token leaked: %v", err)
	}
	for _, allowed := range []string{"https://codeload.github.com/o/r/tar.gz/x", "https://ghe.internal/x"} {
		parsed, _ := url.Parse(allowed)
		if err := validateArchiveURL(parsed, "ghe.internal"); err != nil {
			t.Errorf("%s: %v", allowed, err)
		}
	}
}
