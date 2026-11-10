package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerify(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !Verify(body, signature, "secret") {
		t.Fatal("valid signature rejected")
	}
	if Verify([]byte("tampered"), signature, "secret") {
		t.Fatal("tampered payload accepted")
	}
	if Verify(body, signature, "") {
		t.Fatal("empty secret accepted")
	}
}

func TestCommentRequestsReview(t *testing.T) {
	event := func(action, body, association, userType string, pullRequest bool) CommentEvent {
		var comment CommentEvent
		comment.Action = action
		comment.Repository.FullName = "acme/api"
		comment.Issue.Number = 7
		if pullRequest {
			comment.Issue.PullRequest = []byte(`{"url":"x"}`)
		}
		comment.Comment.Body, comment.Comment.AuthorAssociation, comment.Comment.User.Type = body, association, userType
		return comment
	}
	for _, test := range []struct {
		name  string
		event CommentEvent
		want  bool
	}{
		{"member", event("created", "/overload review", "MEMBER", "User", true), true},
		{"command with a note", event("created", "  /Overload Review\nplease look again", "OWNER", "User", true), true},
		{"outside contributor", event("created", "/overload review", "CONTRIBUTOR", "User", true), false},
		{"issue, not pull request", event("created", "/overload review", "MEMBER", "User", false), false},
		{"edited comment", event("edited", "/overload review", "MEMBER", "User", true), false},
		{"bot", event("created", "/overload review", "MEMBER", "Bot", true), false},
		{"mention in text", event("created", "please /overload review", "MEMBER", "User", true), false},
	} {
		if got := test.event.RequestsReview(); got != test.want {
			t.Errorf("%s: RequestsReview = %v", test.name, got)
		}
	}
	if _, err := ParseComment([]byte(`{"action":"created","repository":{"full_name":"acme/api"},"issue":{"number":7,"pull_request":{}},"comment":{"body":"/overload review","author_association":"MEMBER","user":{"type":"User"}}}`)); err != nil {
		t.Fatal(err)
	}
}
