package postgres

import (
	"context"
	"time"
)

type ChartValue struct {
	Label string
	Count int
}

type DashboardStats struct {
	Available bool
	Since     time.Time
	Until     time.Time
	Total     int
	Completed int
	Failed    int
	Active    int
	Days      []ChartValue
	Outcomes  []ChartValue
	Kinds     []ChartValue
}

func (s *Store) DashboardStatistics(ctx context.Context, now time.Time) (DashboardStats, error) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	stats := DashboardStats{Available: true, Since: today.AddDate(0, 0, -13), Until: now}
	for index := 0; index < 14; index++ {
		stats.Days = append(stats.Days, ChartValue{Label: stats.Since.AddDate(0, 0, index).Format("Jan 02")})
	}
	rows, err := s.Pool.Query(ctx, `SELECT (created_at AT TIME ZONE 'UTC')::date::text,status,kind,count(*) FROM runs WHERE created_at >= $1 AND created_at <= $2 GROUP BY 1,2,3 ORDER BY 1,2,3`, stats.Since, now)
	if err != nil {
		return DashboardStats{}, err
	}
	defer rows.Close()
	outcomes := map[string]int{}
	kinds := map[string]int{}
	for rows.Next() {
		var day, status, kind string
		var count int
		if err := rows.Scan(&day, &status, &kind, &count); err != nil {
			return DashboardStats{}, err
		}
		date, err := time.Parse("2006-01-02", day)
		if err != nil {
			return DashboardStats{}, err
		}
		index := int(date.Sub(stats.Since).Hours() / 24)
		if index >= 0 && index < len(stats.Days) {
			stats.Days[index].Count += count
		}
		stats.Total += count
		outcomes[status] += count
		kinds[kind] += count
	}
	if err := rows.Err(); err != nil {
		return DashboardStats{}, err
	}
	for _, status := range []string{"queued", "running", "completed", "failed", "superseded"} {
		stats.Outcomes = append(stats.Outcomes, ChartValue{Label: status, Count: outcomes[status]})
		delete(outcomes, status)
	}
	other := 0
	for _, count := range outcomes {
		other += count
	}
	if other > 0 {
		stats.Outcomes = append(stats.Outcomes, ChartValue{Label: "other", Count: other})
	}
	for _, kind := range []string{"pr_review", "scheduled_prompt"} {
		stats.Kinds = append(stats.Kinds, ChartValue{Label: kind, Count: kinds[kind]})
		delete(kinds, kind)
	}
	other = 0
	for _, count := range kinds {
		other += count
	}
	if other > 0 {
		stats.Kinds = append(stats.Kinds, ChartValue{Label: "other", Count: other})
	}
	stats.Completed = stats.Outcomes[2].Count
	stats.Failed = stats.Outcomes[3].Count
	stats.Active = stats.Outcomes[0].Count + stats.Outcomes[1].Count
	return stats, nil
}
