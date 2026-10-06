package review

import (
	"encoding/json"

	"github.com/daltoniam/overload"
)

// maxDroppedFindings bounds how many dropped findings a review may store.
const maxDroppedFindings = 200

// CheckResult re-validates a review result before it is stored. Results from
// a sandbox are untrusted, so this applies to both the posted and the dropped
// findings: every finding must be commentable in the diff and its text is
// sanitized; kept findings carry no drop reason and every dropped finding has
// one, so a dropped finding can never be stored as postable. The routing in
// the metrics must decode, or it is removed.
func CheckResult(result overload.ReviewResult, patch string, limit int) (overload.ReviewResult, error) {
	findings, err := Validate(result.Findings, patch, limit)
	if err != nil {
		return result, err
	}
	for index := range findings {
		findings[index].DropReason = ""
	}
	dropped, err := Validate(result.Dropped, patch, maxDroppedFindings)
	if err != nil {
		return result, err
	}
	for index := range dropped {
		reason := sanitize(dropped[index].DropReason, 500)
		if reason == "" {
			reason = "dropped"
		}
		dropped[index].DropReason = reason
	}
	result.Findings, result.Dropped = findings, dropped
	if raw, ok := result.Metrics["routing"]; ok {
		data, err := json.Marshal(raw)
		var routing overload.Routing
		if err != nil || json.Unmarshal(data, &routing) != nil {
			delete(result.Metrics, "routing")
		} else {
			result.Metrics["routing"] = routing
		}
	}
	return result, nil
}
