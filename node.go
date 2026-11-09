package overload

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// NodeReport is what a machine running models reports about itself.
type NodeReport struct {
	Name            string        `json:"name"`
	OS              string        `json:"os"`
	OSVersion       string        `json:"os_version"`
	Chip            string        `json:"chip"`
	MemoryBytes     int64         `json:"memory_bytes"`
	DiskFreeBytes   int64         `json:"disk_free_bytes"`
	OverloadVersion string        `json:"overload_version"`
	LlamaBuild      int           `json:"llama_build,omitempty"`
	MLXVersion      string        `json:"mlx_version,omitempty"`
	Services        []NodeService `json:"services,omitempty"`
	Downloads       []string      `json:"downloads,omitempty"`
}

// NodeService is a model server a node runs.
type NodeService struct {
	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`
	Health   string `json:"health"`
}

// Node is a reporting machine as the control plane sees it.
type Node struct {
	Report   NodeReport
	LastSeen time.Time
}

// NodeOfflineAfter is how long a node may go without reporting before it
// is shown offline.
const NodeOfflineAfter = 5 * time.Minute

// Online reports whether the node reported recently.
func (node Node) Online(now time.Time) bool { return now.Sub(node.LastSeen) <= NodeOfflineAfter }

var nodeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Validate checks a report before it is stored.
func (report NodeReport) Validate() error {
	if !nodeName.MatchString(report.Name) {
		return errors.New("invalid node name")
	}
	for _, text := range []string{report.OS, report.OSVersion, report.Chip, report.OverloadVersion, report.MLXVersion} {
		if len(text) > 200 || strings.ContainsAny(text, "\r\n\x00") {
			return errors.New("report fields must be short single lines")
		}
	}
	if report.MemoryBytes < 0 || report.DiskFreeBytes < 0 || report.LlamaBuild < 0 || len(report.Services) > 50 || len(report.Downloads) > 200 {
		return errors.New("invalid report")
	}
	for _, service := range report.Services {
		if len(service.Model) > 100 || len(service.Endpoint) > 200 || len(service.Health) > 20 {
			return errors.New("invalid service in report")
		}
	}
	for _, download := range report.Downloads {
		if len(download) > 200 {
			return errors.New("invalid download in report")
		}
	}
	return nil
}

// Drift lists, for each runtime setting, the online nodes whose value
// differs from the most common one, so a mixed fleet is easy to spot.
// Clusters need the same versions on every node.
func Drift(nodes []Node, now time.Time) map[string][]string {
	values := map[string]func(NodeReport) string{
		"os_version":       func(report NodeReport) string { return report.OSVersion },
		"overload_version": func(report NodeReport) string { return report.OverloadVersion },
		"llama_build": func(report NodeReport) string {
			if report.LlamaBuild == 0 {
				return ""
			}
			return fmt.Sprint(report.LlamaBuild)
		},
		"mlx_version": func(report NodeReport) string { return report.MLXVersion },
	}
	drift := map[string][]string{}
	for field, read := range values {
		counts := map[string]int{}
		for _, node := range nodes {
			if value := read(node.Report); node.Online(now) && value != "" {
				counts[value]++
			}
		}
		if len(counts) < 2 {
			continue
		}
		common, best := "", 0
		for value, count := range counts {
			if count > best || (count == best && value > common) {
				common, best = value, count
			}
		}
		for _, node := range nodes {
			if value := read(node.Report); node.Online(now) && value != "" && value != common {
				drift[field] = append(drift[field], node.Report.Name)
			}
		}
		slices.Sort(drift[field])
	}
	return drift
}
