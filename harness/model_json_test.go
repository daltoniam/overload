package harness

import "testing"

func TestCleanModelJSON(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{`{"summary":"ok"}`, `{"summary":"ok"}`},
		{"```json\n{\"summary\":\"ok\"}\n```", `{"summary":"ok"}`},
		{"```JSON\n{\"findings\":[]}\n```", `{"findings":[]}`},
		{"```\n{\"summary\":\"ok\"}\n```", `{"summary":"ok"}`},
		{"```go\n{\"summary\":\"ok\"}\n```", "```go\n{\"summary\":\"ok\"}\n```"},
		{"```json\n{\"summary\":\"ok\"}", "```json\n{\"summary\":\"ok\"}"},
	} {
		if got := cleanModelJSON(test.input); got != test.want {
			t.Errorf("got %q want %q", got, test.want)
		}
	}
}
