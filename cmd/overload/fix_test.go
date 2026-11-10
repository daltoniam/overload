package main

import (
	"strings"
	"testing"
)

func TestParseFixOptions(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		err  string
	}{
		{"defaults", []string{"--workflow", "cohort"}, ""},
		{"no workflow", []string{}, "usage"},
		{"zero timeout", []string{"--workflow", "cohort", "--timeout", "0"}, "timeout"},
		{"long timeout", []string{"--workflow", "cohort", "--timeout", "6h"}, "timeout"},
		{"zero rounds", []string{"--workflow", "cohort", "--rounds", "0"}, "rounds"},
		{"bad severity", []string{"--workflow", "cohort", "--until", "urgent"}, "severity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseFixOptions(test.args)
			if test.err == "" && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if test.err != "" && (err == nil || !strings.Contains(err.Error(), test.err)) {
				t.Fatalf("error = %v, want %q", err, test.err)
			}
		})
	}
}
