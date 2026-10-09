package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestManifest(t *testing.T) {
	data, err := Manifest("overload-local", "http://127.0.0.1:8082/", "https://hooks.example.com/webhooks/github")
	if err != nil {
		t.Fatal(err)
	}
	var parsed manifest
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.RedirectURL != "http://127.0.0.1:8082/setup/github/callback" || parsed.HookAttributes["url"] != "https://hooks.example.com/webhooks/github" || parsed.Public {
		t.Fatalf("manifest %+v", parsed)
	}
	if parsed.DefaultPermissions["pull_requests"] != "write" || parsed.DefaultPermissions["contents"] != "read" || len(parsed.DefaultPermissions) != 3 {
		t.Fatalf("permissions %+v", parsed.DefaultPermissions)
	}
	for _, bad := range [][3]string{
		{"x", "ftp://host", "https://hooks.example.com"},
		{"x", "http://127.0.0.1:8082", "http://hooks.example.com"},
		{"x", "http://127.0.0.1:8082", "https://user:pw@hooks.example.com"},
		{"", "http://127.0.0.1:8082", "https://hooks.example.com"},
		{strings.Repeat("a", 35), "http://127.0.0.1:8082", "https://hooks.example.com"},
	} {
		if _, err := Manifest(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestManifestFormAction(t *testing.T) {
	if got, _ := ManifestFormAction(""); got != "https://github.com/settings/apps/new" {
		t.Error(got)
	}
	if got, _ := ManifestFormAction("acme-co"); got != "https://github.com/organizations/acme-co/settings/apps/new" {
		t.Error(got)
	}
	if _, err := ManifestFormAction("acme/../x"); err == nil {
		t.Error("accepted path in organization")
	}
}

func TestCompleteManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app-manifests/abc123/conversions" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":42,"slug":"overload-local","html_url":"https://github.com/apps/overload-local","pem":"-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----\n","webhook_secret":"s3cret","owner":{"login":"acme","type":"Organization"}}`))
	}))
	defer server.Close()
	credentials, err := CompleteManifest(context.Background(), "abc123", server.URL)
	if err != nil || credentials.AppID != 42 || credentials.WebhookSecret != "s3cret" || credentials.Slug != "overload-local" {
		t.Fatalf("%+v %v", credentials, err)
	}
	if credentials.SettingsURL() != "https://github.com/organizations/acme/settings/apps/overload-local" {
		t.Fatalf("organization settings URL %q", credentials.SettingsURL())
	}
	if personal := (AppCredentials{Slug: "overload-local", OwnerType: "User", OwnerLogin: "dalton"}); personal.SettingsURL() != "https://github.com/settings/apps/overload-local" {
		t.Fatalf("personal settings URL %q", personal.SettingsURL())
	}
	if _, err := CompleteManifest(context.Background(), "../x", server.URL); err == nil {
		t.Fatal("accepted path in code")
	}
}

func TestParseInstallation(t *testing.T) {
	event, err := ParseInstallation([]byte(`{"action":"added","installation":{"id":7,"account":{"login":"acme","type":"Organization"}},"repositories_added":[{"id":11,"full_name":"acme/api"}]}`))
	if err != nil || event.Installation.ID != 7 || len(event.RepositoriesAdded) != 1 || event.RepositoriesAdded[0].FullName != "acme/api" {
		t.Fatalf("%+v %v", event, err)
	}
	for _, bad := range []string{`{"action":"created"}`, `{"action":"added","installation":{"id":7},"repositories_added":[{"id":0,"full_name":"acme/api"}]}`, `{"action":"added","installation":{"id":7},"repositories_added":[{"id":3,"full_name":"bad"}]}`} {
		if _, err := ParseInstallation([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestInstallationSourceUsesInstallationToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	minted := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				t.Error("token request without app JWT")
			}
			minted++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`))
		case r.URL.Path == "/repos/acme/api/pulls/5":
			if r.Header.Get("Authorization") != "token installation-token" {
				t.Errorf("PR fetched with %q", r.Header.Get("Authorization"))
			}
			if strings.Contains(r.Header.Get("Accept"), "diff") {
				_, _ = w.Write([]byte("diff --git a/x b/x\n"))
				return
			}
			_, _ = w.Write([]byte(`{"number":5,"head":{"sha":"` + strings.Repeat("a", 40) + `"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(9, pemKey)
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	pr, diff, err := InstallationSource{Client: client, InstallationID: 7}.PullRequestWithDiff(context.Background(), "acme/api", 5)
	if err != nil || pr.GetNumber() != 5 || !strings.HasPrefix(diff, "diff --git") || minted == 0 {
		t.Fatalf("pr=%v diff=%q minted=%d err=%v", pr, diff, minted, err)
	}
	if _, _, err := (InstallationSource{Client: client}).PullRequestWithDiff(context.Background(), "acme/api", 5); err == nil {
		t.Fatal("accepted missing installation")
	}
}

func TestFindReviewIgnoresReviewsByOthers(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	marker := ReviewMarker(41)
	reviews := `[{"id":1,"user":{"login":"mallory"},"body":"looks fine ` + marker + `"},{"id":2,"user":{"login":"overload-test[bot]"},"body":"` + marker + ` then more text"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app":
			_, _ = w.Write([]byte(`{"slug":"overload-test"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`))
		case r.URL.Path == "/repos/acme/api/pulls/5/reviews":
			_, _ = w.Write([]byte(reviews + "]"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(9, pemKey)
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	if id, err := client.FindReview(context.Background(), 7, "acme/api", 5, marker); err != nil || id != 0 {
		t.Fatalf("trusted a forged or misplaced marker: id=%d err=%v", id, err)
	}
	reviews += `,{"id":3,"user":{"login":"overload-test[bot]"},"body":"Overload found 1 issue(s).\n\n` + marker + `"}`
	if id, err := client.FindReview(context.Background(), 7, "acme/api", 5, marker); err != nil || id != 3 {
		t.Fatalf("own review not found: id=%d err=%v", id, err)
	}
}

func TestPostReviewRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Unprocessable Entity","errors":["commit_id is not part of the pull request"]}`))
	}))
	defer server.Close()
	client, err := NewTokenClient("token")
	if err != nil {
		t.Fatal(err)
	}
	if client.api.BaseURL, err = url.Parse(server.URL + "/"); err != nil {
		t.Fatal(err)
	}
	_, err = client.PostReview(context.Background(), 0, "acme/api", 5, strings.Repeat("a", 40), "COMMENT", "summary", nil)
	if !errors.Is(err, ErrReviewRejected) {
		t.Fatalf("422 not reported as a rejection: %v", err)
	}
}

func TestSetWebhookURL(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/app/hook/config" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("unexpected %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := NewClient(9, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	if err := client.SetWebhookURL(context.Background(), "https://overload.example.com/webhooks/github"); err != nil {
		t.Fatal(err)
	}
	if got["url"] != "https://overload.example.com/webhooks/github" || got["content_type"] != "json" {
		t.Fatalf("hook config %v", got)
	}
}

func TestChangesRequestedAndDismissOnlyOwnReviews(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	var dismissed, message string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app":
			_, _ = w.Write([]byte(`{"slug":"overload-test"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/pulls/5/reviews":
			_, _ = w.Write([]byte(`[{"id":1,"user":{"login":"mallory"},"state":"CHANGES_REQUESTED"},{"id":2,"user":{"login":"overload-test[bot]"},"state":"COMMENTED"},{"id":3,"user":{"login":"overload-test[bot]"},"state":"CHANGES_REQUESTED"},{"id":4,"user":{"login":"overload-test[bot]"},"state":"DISMISSED"}]`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/api/pulls/5/reviews/3/dismissals":
			var body struct {
				Message string `json:"message"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			dismissed, message = "3", body.Message
			_, _ = w.Write([]byte(`{"id":3,"state":"DISMISSED"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(9, pemKey)
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	ids, err := client.ChangesRequested(context.Background(), 7, "acme/api", 5)
	if err != nil || len(ids) != 1 || ids[0] != 3 {
		t.Fatalf("ids=%v err=%v; want only overload's own standing request", ids, err)
	}
	if err := client.DismissReview(context.Background(), 7, "acme/api", 5, 3, "nothing blocking"); err != nil || dismissed != "3" || message != "nothing blocking" {
		t.Fatalf("dismiss: %v %q %q", err, dismissed, message)
	}
}
