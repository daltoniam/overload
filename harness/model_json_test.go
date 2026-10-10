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

func TestDecodeModelReviewToleratesQuotedNumbers(t *testing.T) {
	tests := []struct {
		name           string
		text           string
		wantLine       int
		wantConfidence float64
		wantErr        bool
	}{
		{"numbers", `{"summary":"s","findings":[{"path":"a.go","line":12,"confidence":0.9}]}`, 12, 0.9, false},
		{"quoted numbers", `{"summary":"s","findings":[{"path":"a.go","line":"12","start_line":"10","confidence":"0.9"}]}`, 12, 0.9, false},
		{"percent confidence", `{"summary":"s","findings":[{"path":"a.go","line":3,"confidence":"85%"}]}`, 3, 0.85, false},
		{"fenced", "```json\n{\"summary\":\"s\",\"findings\":[{\"path\":\"a.go\",\"line\":\"7\",\"confidence\":\"0.5\"}]}\n```", 7, 0.5, false},
		{"word confidence", `{"summary":"s","findings":[{"path":"a.go","line":3,"confidence":"high"}]}`, 0, 0, true},
		{"word line", `{"summary":"s","findings":[{"path":"a.go","line":"twelve","confidence":0.9}]}`, 0, 0, true},
		{"not json", `here are my findings`, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := decodeModelReview(tt.text)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("accepted %s", tt.text)
				}
				return
			}
			if err != nil || len(result.Findings) != 1 || result.Findings[0].Line != tt.wantLine || result.Findings[0].Confidence != tt.wantConfidence {
				t.Fatalf("result %+v err %v", result, err)
			}
		})
	}
}
