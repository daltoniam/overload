package pages

import (
	"testing"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/eval"
	"github.com/daltoniam/overload/postgres"
)

func TestSummarize(t *testing.T) {
	model := func(name string) postgres.BakeoffModel {
		return postgres.BakeoffModel{ModelProfile: overload.ModelProfile{Name: name}}
	}
	found := func(name string) postgres.BakeoffResult {
		return postgres.BakeoffResult{Model: name, Score: eval.Score{Expected: 1, Matched: 1}}
	}
	bakeoff := postgres.Bakeoff{
		Models: []postgres.BakeoffModel{model("once"), model("refused"), model("often"), model("missed")},
		Results: []postgres.BakeoffResult{
			found("once"),
			{Model: "refused", Error: "refused by the model policy", Score: eval.Score{Expected: 1}},
			found("often"), found("often"), found("often"),
			{Model: "missed", Score: eval.Score{Expected: 1}},
		},
	}
	var order, shown []string
	for _, summary := range Summarize(bakeoff) {
		order = append(order, summary.Model)
		shown = append(shown, summary.Found())
	}
	wantOrder := []string{"often", "once", "missed", "refused"}
	wantShown := []string{"3 of 3 (3 reviews)", "1 of 1 (1 review)", "0 of 1 (1 review)", "n/a"}
	for index := range wantOrder {
		if order[index] != wantOrder[index] || shown[index] != wantShown[index] {
			t.Fatalf("order %v, shown %v", order, shown)
		}
	}
}
