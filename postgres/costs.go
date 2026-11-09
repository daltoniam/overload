package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5/pgconn"
)

// setRunCostSQL sets a run's total cost ($2 dollars, $3 unpriced calls)
// and records the difference from what was recorded before at $4.
// The update reads the locked row's old values through FROM old, so they
// are captured before the write.
const setRunCostSQL = `WITH old AS (SELECT id, cost_usd, cost_unpriced_calls FROM runs WHERE id=$1 FOR UPDATE),
updated AS (UPDATE runs r SET cost_usd=$2::float8, cost_unpriced_calls=$3::bigint FROM old WHERE r.id = old.id
  RETURNING r.id, old.cost_usd AS old_usd, old.cost_unpriced_calls AS old_unpriced)
INSERT INTO run_costs (run_id, usd, unpriced_calls, at)
SELECT id, $2::float8 - COALESCE(old_usd, 0), $3::bigint - old_unpriced, $4::timestamptz FROM updated
WHERE old_usd IS NULL OR $2::float8 <> old_usd OR $3::bigint <> old_unpriced`

// addRunCostSQL adds to a run's cost and records the addition at $4.
const addRunCostSQL = `WITH updated AS (UPDATE runs SET cost_usd=COALESCE(cost_usd,0)+$2::float8, cost_unpriced_calls=cost_unpriced_calls+$3::bigint WHERE id=$1 RETURNING id)
INSERT INTO run_costs (run_id, usd, unpriced_calls, at) SELECT id, $2::float8, $3::bigint, $4::timestamptz FROM updated`

// SetRunCost records what a run's model calls cost in total.
func (s *Store) SetRunCost(ctx context.Context, runID int64, spend overload.Spend) error {
	return s.SetRunCostIn(ctx, s.Pool, runID, spend)
}

// SetRunCostIn is SetRunCost within a transaction. Only spend not
// recorded before counts toward the current period.
func (s *Store) SetRunCostIn(ctx context.Context, q execer, runID int64, spend overload.Spend) error {
	if err := checkSpend(spend); err != nil {
		return err
	}
	_, err := q.Exec(ctx, setRunCostSQL, runID, spend.USD, spend.Unpriced, s.now())
	return err
}

// AddRunCost adds to what a run's model calls cost, for runs whose calls
// are recorded as they finish.
func (s *Store) AddRunCost(ctx context.Context, runID int64, spend overload.Spend) error {
	if err := checkSpend(spend); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, addRunCostSQL, runID, spend.USD, spend.Unpriced, s.now())
	return err
}

// execer runs a statement on a pool or in a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func checkSpend(spend overload.Spend) error {
	if math.IsNaN(spend.USD) || math.IsInf(spend.USD, 0) || spend.USD < 0 || spend.Unpriced < 0 {
		return errors.New("invalid run cost")
	}
	return nil
}

// CostFilter selects runs for a cost report. Repository is owner/name;
// empty means all.
type CostFilter struct {
	Since, Until time.Time
	Repository   string
}

// CostReport sums what runs cost. Runs counts runs with a recorded cost;
// Average and P90 are per such run.
type CostReport struct {
	Since         time.Time `json:"since"`
	Until         time.Time `json:"until"`
	Repository    string    `json:"repository,omitempty"`
	Runs          int       `json:"runs"`
	TotalUSD      float64   `json:"total_usd"`
	AverageUSD    float64   `json:"average_usd"`
	P50USD        float64   `json:"p50_usd"`
	P90USD        float64   `json:"p90_usd"`
	UnpricedCalls int64     `json:"unpriced_calls"`
	// UnpricedRuns made calls to models without prices; their cost in
	// dollars is unknown and not in the totals.
	UnpricedRuns int `json:"unpriced_runs"`
}

// RunCosts reports the cost of runs created in the filter's window.
func (s *Store) RunCosts(ctx context.Context, filter CostFilter) (CostReport, error) {
	report := CostReport{Since: filter.Since, Until: filter.Until, Repository: filter.Repository}
	if filter.Until.IsZero() {
		report.Until = time.Now().UTC()
	}
	rows, err := s.Pool.Query(ctx, `SELECT r.cost_usd, r.cost_unpriced_calls FROM runs r LEFT JOIN repositories repo ON repo.id=r.repository_id WHERE r.cost_usd IS NOT NULL AND r.created_at >= $1 AND r.created_at < $2 AND ($3='' OR repo.full_name=$3)`, report.Since, report.Until, filter.Repository)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	var costs []float64
	for rows.Next() {
		var usd float64
		var unpriced int64
		if err := rows.Scan(&usd, &unpriced); err != nil {
			return report, err
		}
		costs = append(costs, usd)
		report.TotalUSD += usd
		report.UnpricedCalls += unpriced
		if unpriced > 0 {
			report.UnpricedRuns++
		}
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	report.Runs = len(costs)
	if report.Runs > 0 {
		report.AverageUSD = report.TotalUSD / float64(report.Runs)
		report.P50USD, report.P90USD = overload.Percentile(costs, 50), overload.Percentile(costs, 90)
	}
	return report, nil
}

// SpendSummary is what the dashboard shows about spend: today and this
// calendar month (UTC), and the average per priced run this month.
type SpendSummary struct {
	Today      float64
	Month      float64
	MonthRuns  int
	AverageRun float64
	Unpriced   int
}

func monthStart(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func dayStart(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// Spend summarizes spend for the dashboard.
func (s *Store) Spend(ctx context.Context, now time.Time) (SpendSummary, error) {
	var summary SpendSummary
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(sum(usd) FILTER (WHERE at >= $2),0), COALESCE(sum(usd),0), count(DISTINCT run_id), count(DISTINCT run_id) FILTER (WHERE unpriced_calls > 0) FROM run_costs WHERE at >= $1 AND at <= $3`, monthStart(now), dayStart(now), now).Scan(&summary.Today, &summary.Month, &summary.MonthRuns, &summary.Unpriced)
	if err != nil {
		return summary, fmt.Errorf("spend: %w", err)
	}
	if summary.MonthRuns > 0 {
		summary.AverageRun = summary.Month / float64(summary.MonthRuns)
	}
	return summary, nil
}

// TransferStep is how long one step of getting the repository to the
// reviewer took across runs, in seconds.
type TransferStep struct {
	Step string  `json:"step"`
	Runs int     `json:"runs"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	Max  float64 `json:"max"`
}

// transferSteps are the timings reviews record under metrics.transfer.
var transferSteps = []string{"fetch_seconds", "sandbox_start_seconds", "upload_seconds", "extract_seconds", "review_seconds", "archive_bytes"}

// TransferTimes summarizes repository transfer timings of runs in the
// filter's window, to decide whether a large repository needs a faster
// transfer. An unset Until means now.
func (s *Store) TransferTimes(ctx context.Context, filter CostFilter) ([]TransferStep, error) {
	var until *time.Time
	if !filter.Until.IsZero() {
		until = &filter.Until
	}
	var steps []TransferStep
	for _, step := range transferSteps {
		summary := TransferStep{Step: step}
		err := s.Pool.QueryRow(ctx, `SELECT count(*), COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY value), 0), COALESCE(percentile_cont(0.9) WITHIN GROUP (ORDER BY value), 0), COALESCE(max(value), 0) FROM (SELECT (r.metrics->'transfer'->>$4)::float8 AS value FROM runs r LEFT JOIN repositories repo ON repo.id=r.repository_id WHERE r.created_at >= $1 AND r.created_at < COALESCE($2::timestamptz, now()) AND ($3='' OR repo.full_name=$3) AND jsonb_typeof(r.metrics->'transfer'->$4) = 'number') timings`, filter.Since, until, filter.Repository, step).Scan(&summary.Runs, &summary.P50, &summary.P90, &summary.Max)
		if err != nil {
			return nil, err
		}
		steps = append(steps, summary)
	}
	return steps, nil
}
