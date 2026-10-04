package eval

import (
	"testing"

	"github.com/daltoniam/overload"
)

func TestHistoricalFindingsMatchOriginalLines(t *testing.T) {
	expected := []ExpectedFinding{
		{Path: "integrations/airflow/compact.yaml", StartLine: 9, EndLine: 9, Description: "DAG scheduling state is lost"},
		{Path: "integrations/airflow/compact.yaml", StartLine: 11, EndLine: 12, Description: "Next-run dates are lost"},
		{Path: "integrations/airflow/airflow.go", StartLine: 157, EndLine: 157, Description: "Refresh depends on status text"},
	}
	found := []overload.Finding{{Path: "integrations/airflow/compact.yaml", Line: 9}, {Path: "integrations/airflow/airflow.go", Line: 157}}
	score := ScoreFindings(expected, found, 0)
	if score.Expected != 3 || score.Matched != 2 || score.Recall != 2.0/3.0 {
		t.Fatalf("historical score=%+v", score)
	}
}
