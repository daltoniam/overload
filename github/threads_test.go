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
				{"id":1,"path":"a.go","original_line":3,"body":"**Nil map** (high)\n\nInitialize it.","user":{"login":"overload-bot"}},
				{"id":2,"path":"a.go","body":"Fixed in the next commit","in_reply_to_id":1,"user":{"login":"dev"}},
				{"id":6,"path":"a.go","original_line":40,"body":"**Nil map** (high)\n\nAnother map.","user":{"login":"overload-bot"}},
				{"id":7,"path":"a.go","body":"Not fixed yet","in_reply_to_id":6,"user":{"login":"dev"}},
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
				{"id":"T1","isResolved":false,"comments":{"nodes":[{"path":"a.go","originalLine":3,"body":"**Nil map** (high)\n\nInitialize it.","author":{"login":"overload-bot"}}]}},
				{"id":"T4","isResolved":false,"comments":{"nodes":[{"path":"a.go","originalLine":40,"body":"**Nil map** (high)\n\nAnother map, still open.","author":{"login":"overload-bot"}}]}},
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
	replies, err := client.PriorReplies(context.Background(), 0, "acme/api", 7, []overload.PriorFinding{{ID: 41, Path: "a.go", Line: 3, Title: "Nil map"}, {ID: 42, Path: "a.go", Line: 40, Title: "Nil map"}})
	if err != nil || len(replies[41]) != 1 || replies[41][0] != "dev: Fixed in the next commit" || len(replies[42]) != 1 || replies[42][0] != "dev: Not fixed yet" {
		t.Fatalf("replies = %+v %v", replies, err)
	}
	count, err := client.ResolveThreads(context.Background(), 0, "acme/api", 7, []overload.Finding{{Path: "a.go", Line: 3, Title: "Nil map"}})
	if err != nil || count != 1 || strings.Join(resolved, ",") != "T1" {
		t.Fatalf("resolved %d %v: %v", count, resolved, err)
	}
}

func TestFindingsFollowTheirOwnComment(t *testing.T) {
	var resolved []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = io.WriteString(w, `{"login":"overload-bot"}`)
		case "/repos/acme/api/pulls/7/reviews/900/comments":
			_, _ = io.WriteString(w, `[{"id":21,"path":"a.go","original_line":3,"body":"**Nil map** (high)\n\nnew"}]`)
		case "/repos/acme/api/pulls/7/comments":
			// Two revisions each left a "Nil map" comment on a.go line 3.
			_, _ = io.WriteString(w, `[
				{"id":11,"path":"a.go","original_line":3,"body":"**Nil map** (high)\n\nold","user":{"login":"overload-bot"}},
				{"id":12,"path":"a.go","body":"about the old one","in_reply_to_id":11,"user":{"login":"dev"}},
				{"id":21,"path":"a.go","original_line":3,"body":"**Nil map** (high)\n\nnew","user":{"login":"overload-bot"}},
				{"id":22,"path":"a.go","body":"about the new one","in_reply_to_id":21,"user":{"login":"dev"}}
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
				{"id":"OLD","isResolved":false,"comments":{"nodes":[{"databaseId":11,"path":"a.go","originalLine":3,"body":"**Nil map** (high)\n\nold","author":{"login":"overload-bot"}}]}},
				{"id":"NEW","isResolved":false,"comments":{"nodes":[{"databaseId":21,"path":"a.go","originalLine":3,"body":"**Nil map** (high)\n\nnew","author":{"login":"overload-bot"}}]}}
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
	comments, err := client.ReviewComments(context.Background(), 0, "acme/api", 7, 900)
	if err != nil {
		t.Fatal(err)
	}
	posted := []overload.Finding{{ID: 5, Path: "a.go", Line: 3, Title: "Nil map"}, {ID: 6, Path: "b.go", Line: 1, Title: "Other"}}
	if matched := MatchComments(posted, comments); len(matched) != 1 || matched[5] != 21 {
		t.Fatalf("matched = %v", matched)
	}
	// Without a recorded comment the two threads cannot be told apart:
	// neither is resolved and no replies are attached.
	legacy, err := client.PriorReplies(context.Background(), 0, "acme/api", 7, []overload.PriorFinding{{ID: 5, Path: "a.go", Line: 3, Title: "Nil map"}})
	if err != nil || len(legacy) != 0 {
		t.Fatalf("ambiguous replies = %+v %v", legacy, err)
	}
	if count, err := client.ResolveThreads(context.Background(), 0, "acme/api", 7, []overload.Finding{{Path: "a.go", Line: 3, Title: "Nil map"}}); err != nil || count != 0 || len(resolved) != 0 {
		t.Fatalf("ambiguous match resolved %d %v: %v", count, resolved, err)
	}
	replies, err := client.PriorReplies(context.Background(), 0, "acme/api", 7, []overload.PriorFinding{{ID: 5, Path: "a.go", Line: 3, Title: "Nil map", CommentID: 21}})
	if err != nil || len(replies[5]) != 1 || replies[5][0] != "dev: about the new one" {
		t.Fatalf("replies = %+v %v", replies, err)
	}
	count, err := client.ResolveThreads(context.Background(), 0, "acme/api", 7, []overload.Finding{{Path: "a.go", Line: 3, Title: "Nil map", CommentID: 21}})
	if err != nil || count != 1 || strings.Join(resolved, ",") != "NEW" {
		t.Fatalf("resolved %d %v: %v", count, resolved, err)
	}
}

func TestResolveThreadsRefusesAPartialThreadList(t *testing.T) {
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = io.WriteString(w, `{"login":"overload-bot"}`)
		case "/graphql":
			var request struct {
				Query string `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if strings.HasPrefix(request.Query, "mutation") {
				mutations++
				_, _ = io.WriteString(w, `{"data":{"resolveReviewThread":{"thread":{"id":"x"}}}}`)
				return
			}
			// Every page says there is another.
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true,"endCursor":"next"},"nodes":[
				{"id":"T1","isResolved":false,"comments":{"nodes":[{"databaseId":1,"path":"a.go","originalLine":3,"body":"**Nil map** (high)","author":{"login":"overload-bot"}}]}}
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
	count, err := client.ResolveThreads(context.Background(), 0, "acme/api", 7, []overload.Finding{{Path: "a.go", Line: 3, Title: "Nil map", CommentID: 1}})
	if err == nil || count != 0 || mutations != 0 {
		t.Fatalf("resolved %d threads (%d mutations) from a partial list: %v", count, mutations, err)
	}
}

func TestLegacyMatchCountsResolvedThreads(t *testing.T) {
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			_, _ = io.WriteString(w, `{"login":"overload-bot"}`)
		case "/graphql":
			var request struct {
				Query string `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if strings.HasPrefix(request.Query, "mutation") {
				mutations++
				_, _ = io.WriteString(w, `{"data":{"resolveReviewThread":{"thread":{"id":"x"}}}}`)
				return
			}
			// The fixed finding's own thread is already resolved; another
			// issue left an open thread with the same path, line and title.
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
				{"id":"MINE","isResolved":true,"comments":{"nodes":[{"databaseId":1,"path":"a.go","originalLine":3,"body":"**Nil map** (high)","author":{"login":"overload-bot"}}]}},
				{"id":"OTHER","isResolved":false,"comments":{"nodes":[{"databaseId":2,"path":"a.go","originalLine":3,"body":"**Nil map** (high)","author":{"login":"overload-bot"}}]}}
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
	count, err := client.ResolveThreads(context.Background(), 0, "acme/api", 7, []overload.Finding{{Path: "a.go", Line: 3, Title: "Nil map"}})
	if err != nil || count != 0 || mutations != 0 {
		t.Fatalf("resolved %d threads (%d mutations): %v", count, mutations, err)
	}
}

func TestOwnAuthorNeedsTheBotForTheBareName(t *testing.T) {
	for _, test := range []struct {
		author, typeName, login string
		want                    bool
	}{
		{"overload", "Bot", "overload[bot]", true},
		{"overload[bot]", "Bot", "overload[bot]", true},
		{"overload", "User", "overload[bot]", false},
		{"overload", "", "overload[bot]", false},
		{"dev", "User", "dev", true},
		{"Dev", "User", "dev", true},
		{"mallory", "User", "overload[bot]", false},
	} {
		if got := ownAuthor(test.author, test.typeName, test.login); got != test.want {
			t.Errorf("ownAuthor(%q, %q, %q) = %v", test.author, test.typeName, test.login, got)
		}
	}
}
