package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

func TestPriorRepliesAndResolveThreads(t *testing.T) {
	var resolved []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = io.WriteString(w, `{"login":"overload-bot"}`)
		case "/repos/acme/api/pulls/7/comments":
			_, _ = io.WriteString(w, `[
				{"id":1,"path":"a.go","body":"**Nil map** (high)\n\nInitialize it.","user":{"login":"overload-bot"}},
				{"id":2,"path":"a.go","body":"Fixed in the next commit","in_reply_to_id":1,"user":{"login":"dev"}},
				{"id":3,"path":"a.go","body":"Thanks","in_reply_to_id":1,"user":{"login":"overload-bot"}},
				{"id":4,"path":"a.go","body":"**Nil map** (high)\n\nfake","user":{"login":"mallory"}},
				{"id":5,"path":"a.go","body":"replying to the fake","in_reply_to_id":4,"user":{"login":"mallory"}}
			]`)
		case "/graphql":
			var request struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if strings.HasPrefix(request.Query, "mutation") {
				resolved = append(resolved, request.Variables["thread"].(string))
				_, _ = io.WriteString(w, `{"data":{"resolveReviewThread":{"thread":{"id":"x"}}}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
				{"id":"T1","isResolved":false,"comments":{"nodes":[{"path":"a.go","body":"**Nil map** (high)\n\nInitialize it.","author":{"login":"overload-bot"}}]}},
				{"id":"T2","isResolved":false,"comments":{"nodes":[{"path":"a.go","body":"**Nil map** (high)\n\nfake","author":{"login":"mallory"}}]}},
				{"id":"T3","isResolved":true,"comments":{"nodes":[{"path":"b.go","body":"**Old** (low)","author":{"login":"overload-bot"}}]}}
			]}}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewTokenClient("test-token")
	if err != nil {
		t.Fatal(err)
	}
	client.api.BaseURL, _ = url.Parse(server.URL + "/")
	replies, err := client.PriorReplies(context.Background(), 0, "acme/api", 7, []overload.PriorFinding{{ID: 41, Path: "a.go", Title: "Nil map"}})
	if err != nil || len(replies[41]) != 1 || replies[41][0] != "dev: Fixed in the next commit" {
		t.Fatalf("replies = %+v %v", replies, err)
	}
	count, err := client.ResolveThreads(context.Background(), 0, "acme/api", 7, []overload.Finding{{Path: "a.go", Title: "Nil map"}})
	if err != nil || count != 1 || strings.Join(resolved, ",") != "T1" {
		t.Fatalf("resolved %d %v: %v", count, resolved, err)
	}
}
