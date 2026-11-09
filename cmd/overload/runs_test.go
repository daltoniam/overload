package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload/postgres"
)

func TestParseCostFlags(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	filter, asJSON, err := parseCostFlags(nil, now)
	if err != nil || asJSON || !filter.Since.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || !filter.Until.Equal(now) {
		t.Fatalf("defaults = %+v %v %v", filter, asJSON, err)
	}
	filter, asJSON, err = parseCostFlags([]string{"--since", "2026-10-01", "--until", "2026-11-01", "--repo", "acme/api", "--json"}, now)
	if err != nil || !asJSON || filter.Repository != "acme/api" || filter.Since.Month() != 10 || filter.Until.Month() != 11 {
		t.Fatalf("flags = %+v %v %v", filter, asJSON, err)
	}
	for _, args := range [][]string{{"--since", "11/01"}, {"--since", "2026-11-05", "--until", "2026-11-01"}, {"extra"}} {
		if _, _, err := parseCostFlags(args, now); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestWriteCostReport(t *testing.T) {
	var out bytes.Buffer
	report := postgres.CostReport{Runs: 4, TotalUSD: 4, AverageUSD: 1, P50USD: 0.75, P90USD: 2, UnpricedRuns: 1, UnpricedCalls: 3}
	if err := writeCostReport(&out, report, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Runs with a recorded cost: 4", "Total: $4.00", "p90: $2.00", "3 calls in 1 run to models without prices"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
	}
}
