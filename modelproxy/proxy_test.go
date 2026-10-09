package modelproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
)

func TestProxyForwardsGrantedModelWithKey(t *testing.T) {
	t.Setenv("OVERLOAD_MODEL_TEST", "secret-key")
	var gotAuth, gotPath, gotHeader, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotHeader = r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("cf-aig-metadata")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "upstream=1")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	proxy := New("http://overload:8083")
	server := httptest.NewServer(proxy)
	defer server.Close()
	model := overload.ModelProfile{Name: "gpt", BaseURL: upstream.URL + "/v1/", Model: "gpt-6.1-sol", APIKeyEnv: "OVERLOAD_MODEL_TEST", Headers: map[string]string{"cf-aig-metadata": `{"tool":"overload"}`}}
	base, revoke := proxy.Grant(model, time.Minute)
	if !strings.HasPrefix(base, "http://overload:8083/v1/m/") {
		t.Fatalf("base %s", base)
	}
	path := strings.TrimPrefix(base, "http://overload:8083")
	post := func(suffix, body string) *http.Response {
		t.Helper()
		response, err := http.Post(server.URL+suffix, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := post(path+"/chat/completions", `{"model":"gpt-6.1-sol","messages":[]}`)
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(raw) != `{"ok":true}` || response.Header.Get("Set-Cookie") != "" {
		t.Fatalf("response %d %s %v", response.StatusCode, raw, response.Header)
	}
	if gotAuth != "Bearer secret-key" || gotPath != "/v1/chat/completions" || gotHeader != `{"tool":"overload"}` || !strings.Contains(gotBody, "gpt-6.1-sol") {
		t.Fatalf("upstream auth=%q path=%q header=%q body=%q", gotAuth, gotPath, gotHeader, gotBody)
	}
	if response := post(path+"/responses", `{"model":"gpt-6.1-sol"}`); response.StatusCode != http.StatusOK {
		t.Fatalf("responses API: %d", response.StatusCode)
	}
	for name, test := range map[string]struct {
		path, body string
		status     int
	}{
		"other model":     {path + "/chat/completions", `{"model":"gpt-4o"}`, http.StatusForbidden},
		"not json":        {path + "/chat/completions", `nope`, http.StatusForbidden},
		"other endpoint":  {path + "/files", `{"model":"gpt-6.1-sol"}`, http.StatusNotFound},
		"traversal":       {path + "/../models", `{"model":"gpt-6.1-sol"}`, http.StatusNotFound},
		"unknown token":   {"/v1/m/deadbeef/chat/completions", `{"model":"gpt-6.1-sol"}`, http.StatusUnauthorized},
		"no token prefix": {"/chat/completions", `{"model":"gpt-6.1-sol"}`, http.StatusNotFound},
	} {
		if response := post(test.path, test.body); response.StatusCode != test.status {
			t.Errorf("%s: %d want %d", name, response.StatusCode, test.status)
		}
	}
	revoke()
	if response := post(path+"/chat/completions", `{"model":"gpt-6.1-sol"}`); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked grant: %d", response.StatusCode)
	}
}

func TestProxyGrantsExpire(t *testing.T) {
	proxy := New("http://overload:8083")
	now := time.Unix(1_800_000_000, 0)
	proxy.now = func() time.Time { return now }
	base, _ := proxy.Grant(overload.ModelProfile{Model: "m", BaseURL: "http://127.0.0.1:1"}, time.Minute)
	token := strings.TrimPrefix(base, "http://overload:8083/v1/m/")
	if _, ok := proxy.lookup(token); !ok {
		t.Fatal("fresh grant missing")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := proxy.lookup(token); ok {
		t.Fatal("expired grant accepted")
	}
	proxy.Grant(overload.ModelProfile{Model: "m"}, time.Minute)
	if len(proxy.grants) != 1 {
		t.Fatalf("expired grants not cleaned up: %d", len(proxy.grants))
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/m/"+token+"/chat/completions", nil)
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET allowed: %d", recorder.Code)
	}
}
