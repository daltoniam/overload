package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/jackc/pgx/v5"
)

// Budget is a spending cap in US dollars. The global budget (no
// Repository) counts every run; a repository budget counts only that
// repository's runs. Nil caps mean no cap. SkipStatus is the commit status
// a review skipped for budget gets: success (the default) so a required
// overload check does not block merges, or error.
type Budget struct {
	Repository string   `json:"repository,omitempty"`
	MonthlyUSD *float64 `json:"monthly_usd,omitempty"`
	DailyUSD   *float64 `json:"daily_usd,omitempty"`
	SkipStatus string   `json:"skip_status,omitempty"`
	// FromEnvironment marks caps set by OVERLOAD_BUDGET_MONTHLY_USD or
	// OVERLOAD_BUDGET_DAILY_USD, which take precedence over the database.
	FromEnvironment bool `json:"from_environment,omitempty"`
}

// Budget skip statuses.
const (
	BudgetSkipSuccess = "success"
	BudgetSkipError   = "error"
)

// SkipBudgetExhausted is the delivery skip reason for a review the budget
// left out.
const SkipBudgetExhausted = "budget_exhausted"

func (budget Budget) validate() error {
	for _, value := range []*float64{budget.MonthlyUSD, budget.DailyUSD} {
		if value != nil && (math.IsNaN(*value) || *value < 0 || *value > 10_000_000) {
			return errors.New("budget caps must be between 0 and 10,000,000 dollars")
		}
	}
	if budget.SkipStatus != "" && budget.SkipStatus != BudgetSkipSuccess && budget.SkipStatus != BudgetSkipError {
		return errors.New("skip status must be success or error")
	}
	return nil
}

// SaveBudget sets the global budget or a repository's. Nil caps clear them.
func (s *Store) SaveBudget(ctx context.Context, budget Budget) error {
	if err := budget.validate(); err != nil {
		return err
	}
	status := budget.SkipStatus
	if status == "" {
		status = BudgetSkipSuccess
	}
	if budget.Repository == "" {
		_, err := s.Pool.Exec(ctx, `INSERT INTO budgets (repository_id, monthly_usd, daily_usd, skip_status) VALUES (NULL, $1, $2, $3) ON CONFLICT ((repository_id IS NULL)) WHERE repository_id IS NULL DO UPDATE SET monthly_usd=EXCLUDED.monthly_usd, daily_usd=EXCLUDED.daily_usd, skip_status=EXCLUDED.skip_status, updated_at=now()`, budget.MonthlyUSD, budget.DailyUSD, status)
		return err
	}
	var repoID int64
	if err := s.Pool.QueryRow(ctx, `SELECT id FROM repositories WHERE full_name=$1`, budget.Repository).Scan(&repoID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s is not a repository overload knows", budget.Repository)
		}
		return err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO budgets (repository_id, monthly_usd, daily_usd, skip_status) VALUES ($1, $2, $3, $4) ON CONFLICT (repository_id) DO UPDATE SET monthly_usd=EXCLUDED.monthly_usd, daily_usd=EXCLUDED.daily_usd, skip_status=EXCLUDED.skip_status, updated_at=now()`, repoID, budget.MonthlyUSD, budget.DailyUSD, status)
	return err
}

// envCap reads a cap from the environment; ok is false when unset.
func envCap(name string) (*float64, bool) {
	text := strings.TrimSpace(os.Getenv(name))
	if text == "" {
		return nil, false
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || value < 0 {
		return nil, false
	}
	return &value, true
}

// Budgets lists the global budget first, then repository budgets by name.
func (s *Store) Budgets(ctx context.Context) ([]Budget, error) {
	return listBudgets(ctx, s.Pool)
}

type rowsQuerier interface {
	querier
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func listBudgets(ctx context.Context, q rowsQuerier) ([]Budget, error) {
	rows, err := q.Query(ctx, `SELECT COALESCE(r.full_name, ''), b.monthly_usd, b.daily_usd, b.skip_status FROM budgets b LEFT JOIN repositories r ON r.id=b.repository_id ORDER BY b.repository_id IS NOT NULL, r.full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var budgets []Budget
	hasGlobal := false
	for rows.Next() {
		var budget Budget
		if err := rows.Scan(&budget.Repository, &budget.MonthlyUSD, &budget.DailyUSD, &budget.SkipStatus); err != nil {
			return nil, err
		}
		hasGlobal = hasGlobal || budget.Repository == ""
		budgets = append(budgets, budget)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !hasGlobal {
		budgets = append([]Budget{{SkipStatus: BudgetSkipSuccess}}, budgets...)
	}
	if monthly, ok := envCap("OVERLOAD_BUDGET_MONTHLY_USD"); ok {
		budgets[0].MonthlyUSD, budgets[0].FromEnvironment = monthly, true
	}
	if daily, ok := envCap("OVERLOAD_BUDGET_DAILY_USD"); ok {
		budgets[0].DailyUSD, budgets[0].FromEnvironment = daily, true
	}
	return budgets, nil
}

// BudgetCheck is the outcome of checking a budget: whether a new review
// fits, and what was counted.
type BudgetCheck struct {
	Exceeded bool
	// Period is "monthly" or "daily"; Scope is "" for the global budget or
	// the repository's name.
	Period     string
	Scope      string
	Spent      float64
	Pending    float64
	Expected   float64
	Cap        float64
	SkipStatus string
}

// Description is the commit status text for a review skipped for budget.
func (check BudgetCheck) Description() string {
	return "Overload skipped: " + check.Period + " review budget reached"
}

// expectedCostSQL estimates what a run will cost from its pinned workflow:
// its per-run cap when set, otherwise the average of the workflow's last
// 20 priced runs, otherwise 0.
const expectedCostSQL = `COALESCE(NULLIF((%[1]s->>'max_run_cost_usd')::float8, 0), (SELECT avg(c.cost_usd) FROM (SELECT p.cost_usd FROM runs p WHERE p.cost_usd IS NOT NULL AND p.status IN ('completed', 'failed') AND p.config_snapshot->>'name' = %[1]s->>'name' ORDER BY p.id DESC LIMIT 20) c), 0)`

// checkBudget decides whether a review expected to cost expected fits
// every budget that applies to the repository: what was spent this month
// (or today, UTC) plus what queued and running runs are expected to cost
// plus this run must stay within each cap.
//
// replaces names the pull request and head being queued: its queued
// reviews of other heads are about to be superseded, so they do not count
// as pending.
func (s *Store) checkBudget(ctx context.Context, q rowsQuerier, repoID int64, replaces pendingReplacement, expected float64) (BudgetCheck, error) {
	budgets, err := listBudgets(ctx, q)
	if err != nil {
		return BudgetCheck{}, err
	}
	var repoName string
	if repoID != 0 {
		if err := q.QueryRow(ctx, `SELECT full_name FROM repositories WHERE id=$1`, repoID).Scan(&repoName); err != nil {
			return BudgetCheck{}, err
		}
	}
	now := s.now()
	for _, budget := range budgets {
		if budget.Repository != "" && budget.Repository != repoName {
			continue
		}
		for _, period := range []struct {
			name  string
			cap   *float64
			since time.Time
		}{{"monthly", budget.MonthlyUSD, monthStart(now)}, {"daily", budget.DailyUSD, dayStart(now)}} {
			if period.cap == nil {
				continue
			}
			check := BudgetCheck{Period: period.name, Scope: budget.Repository, Cap: *period.cap, Expected: expected, SkipStatus: budget.SkipStatus}
			scoped := budget.Repository != ""
			if err := q.QueryRow(ctx, `SELECT COALESCE(sum(cost_usd), 0) FROM runs WHERE cost_usd IS NOT NULL AND `+spentAt+` >= $1 AND `+spentAt+` <= $2 AND (NOT $3 OR repository_id = $4)`, period.since, now, scoped, repoID).Scan(&check.Spent); err != nil {
				return BudgetCheck{}, err
			}
			pending := fmt.Sprintf(`SELECT COALESCE(sum(GREATEST(`+expectedCostSQL+` - COALESCE(r.cost_usd, 0), 0)), 0) FROM runs r WHERE r.status IN ('queued', 'running') AND r.kind = 'pr_review' AND (NOT $1 OR r.repository_id = $2) AND NOT (r.status = 'queued' AND r.repository_id = $2 AND r.pr_number = $3 AND r.head_sha <> $4)`, "r.config_snapshot")
			if err := q.QueryRow(ctx, pending, scoped, repoID, replaces.pr, replaces.head).Scan(&check.Pending); err != nil {
				return BudgetCheck{}, err
			}
			if check.Spent+check.Pending+expected > check.Cap || check.Spent >= check.Cap {
				check.Exceeded = true
				return check, nil
			}
		}
	}
	return BudgetCheck{}, nil
}

// CheckBudget reports whether a review of a repository expected to cost
// expected dollars fits its budgets.
func (s *Store) CheckBudget(ctx context.Context, repoID int64, expected float64) (BudgetCheck, error) {
	return s.checkBudget(ctx, s.Pool, repoID, pendingReplacement{}, expected)
}

// pendingReplacement is the pull request and head a new review is for.
type pendingReplacement struct {
	pr   int
	head string
}

// expectedRunCost estimates what a review with the pinned workflow will
// cost.
func expectedRunCost(ctx context.Context, q querier, snapshot []byte) (float64, error) {
	var expected float64
	err := q.QueryRow(ctx, fmt.Sprintf(`SELECT `+expectedCostSQL, "$1::jsonb"), snapshot).Scan(&expected)
	return expected, err
}

// BudgetStatus is how much of the global budgets is used, for the
// dashboard and alerts.
type BudgetStatus struct {
	MonthlyUSD   *float64
	DailyUSD     *float64
	MonthSpent   float64
	DaySpent     float64
	MonthPercent float64
	DayPercent   float64
}

// Alert returns the highest threshold (80 or 100 percent) a budget has
// passed, and which one, or 0.
func (status BudgetStatus) Alert() (int, string) {
	switch {
	case status.MonthlyUSD != nil && status.MonthPercent >= 100:
		return 100, "monthly"
	case status.DailyUSD != nil && status.DayPercent >= 100:
		return 100, "daily"
	case status.MonthlyUSD != nil && status.MonthPercent >= 80:
		return 80, "monthly"
	case status.DailyUSD != nil && status.DayPercent >= 80:
		return 80, "daily"
	}
	return 0, ""
}

// GlobalBudgetStatus reports spend against the global budget.
func (s *Store) GlobalBudgetStatus(ctx context.Context) (BudgetStatus, error) {
	budgets, err := s.Budgets(ctx)
	if err != nil {
		return BudgetStatus{}, err
	}
	status := BudgetStatus{MonthlyUSD: budgets[0].MonthlyUSD, DailyUSD: budgets[0].DailyUSD}
	spend, err := s.Spend(ctx, s.now())
	if err != nil {
		return status, err
	}
	status.MonthSpent, status.DaySpent = spend.Month, spend.Today
	if status.MonthlyUSD != nil && *status.MonthlyUSD > 0 {
		status.MonthPercent = 100 * spend.Month / *status.MonthlyUSD
	} else if status.MonthlyUSD != nil {
		status.MonthPercent = 100
	}
	if status.DailyUSD != nil && *status.DailyUSD > 0 {
		status.DayPercent = 100 * spend.Today / *status.DailyUSD
	} else if status.DailyUSD != nil {
		status.DayPercent = 100
	}
	return status, nil
}

// SkipStatusArgs sets a commit status on a head that was not reviewed, for
// example because the budget was reached.
type SkipStatusArgs struct {
	Repository     string `json:"repository"`
	InstallationID int64  `json:"installation_id"`
	SHA            string `json:"sha"`
	State          string `json:"state"`
	Description    string `json:"description"`
}

func (SkipStatusArgs) Kind() string { return "skip_status" }

// SpendExceeded reports whether this month's or today's global spend plus
// pending is past a cap by more than margin (a fraction), for the model
// proxy's backstop. Repository budgets are enforced when reviews are
// queued.
func (s *Store) SpendExceeded(ctx context.Context, pending, margin float64) (bool, string, error) {
	status, err := s.GlobalBudgetStatus(ctx)
	if err != nil {
		return false, "", err
	}
	if status.MonthlyUSD != nil && status.MonthSpent+pending > *status.MonthlyUSD*(1+margin) {
		return true, "monthly", nil
	}
	if status.DailyUSD != nil && status.DaySpent+pending > *status.DailyUSD*(1+margin) {
		return true, "daily", nil
	}
	return false, "", nil
}

// ExpectedCost reports what a review of the named workflow is expected to
// cost, for previews.
func (s *Store) ExpectedCost(ctx context.Context, workflow overload.ResolvedWorkflow) (float64, error) {
	snapshot := fmt.Sprintf(`{"name":%q,"max_run_cost_usd":%v}`, workflow.Name, workflow.MaxRunCostUSD)
	return expectedRunCost(ctx, s.Pool, []byte(snapshot))
}
