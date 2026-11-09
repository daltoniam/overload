package queue

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/daltoniam/overload/modelproxy"
	"github.com/daltoniam/overload/postgres"
)

// BudgetMargin is how far past a global cap the model proxy lets calls go
// before refusing them: reviews already running finish their current
// calls, and the queue-time check keeps new reviews out first.
const BudgetMargin = 0.05

// BudgetGate is the model proxy's backstop: it refuses calls once this
// month's or today's global spend, plus what proxied calls cost that runs
// have not recorded yet, is past a cap by more than margin. Recorded spend
// is read at most every refresh.
func BudgetGate(store *postgres.Store, margin float64, refresh time.Duration) modelproxy.BudgetGate {
	var mu sync.Mutex
	var checked time.Time
	var status postgres.BudgetStatus
	return func(ctx context.Context, pending float64) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(checked) > refresh {
			current, err := store.GlobalBudgetStatus(ctx)
			if err != nil {
				return "", err
			}
			status, checked = current, time.Now()
		}
		switch {
		case status.MonthlyUSD != nil && status.MonthSpent+pending > *status.MonthlyUSD*(1+margin):
			return "monthly", nil
		case status.DailyUSD != nil && status.DaySpent+pending > *status.DailyUSD*(1+margin):
			return "daily", nil
		}
		return "", nil
	}
}

// BudgetAlerts logs once per month when global spend passes 80% and 100%
// of a cap.
type BudgetAlerts struct {
	mu     sync.Mutex
	logged map[string]bool
}

// NewBudgetAlerts returns an alert logger for CheckBudgetAlerts.
func NewBudgetAlerts() *BudgetAlerts {
	return &BudgetAlerts{logged: map[string]bool{}}
}

// Check logs a warning the first time spend passes a threshold in a
// period.
func (alerts *BudgetAlerts) Check(ctx context.Context, store *postgres.Store, now time.Time) {
	status, err := store.GlobalBudgetStatus(ctx)
	if err != nil {
		slog.Warn("budget check failed", "error", err)
		return
	}
	threshold, period := status.Alert()
	if threshold == 0 {
		return
	}
	key := now.UTC().Format("2006-01") + period + string(rune('0'+threshold/20))
	if period == "daily" {
		key = now.UTC().Format("2006-01-02") + period + string(rune('0'+threshold/20))
	}
	alerts.mu.Lock()
	defer alerts.mu.Unlock()
	if alerts.logged[key] {
		return
	}
	alerts.logged[key] = true
	spent, limit := status.MonthSpent, status.MonthlyUSD
	if period == "daily" {
		spent, limit = status.DaySpent, status.DailyUSD
	}
	slog.Warn("review budget alert", "period", period, "percent", threshold, "spent_usd", spent, "cap_usd", *limit)
}
