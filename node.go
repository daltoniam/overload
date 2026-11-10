package overload

import (
	"errors"
	"fmt"
	"maps"
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
	// KeyPrefix identifies the API key that holds the name; empty when no
	// valid key does, so the next key to report takes it.
	KeyPrefix string
}

// NodeOfflineAfter is how long a node may go without reporting before it
// is shown offline.
const NodeOfflineAfter = 5 * time.Minute

// Limits on a node report. Nodes trim what they collect to these, so a
// machine with many downloads still reports instead of being refused.
const (
	MaxNodeTextLength     = 200
	MaxNodeServices       = 50
	MaxNodeDownloads      = 200
	MaxNodeServiceModel   = 100
	MaxNodeEndpointLength = 200
	MaxNodeHealthLength   = 20
	MaxNodeDownloadLength = 200
)

// Node inventory errors. ErrInvalidNodeReport wraps every validation
// failure, so callers can tell a bad report from a storage failure.
var (
	ErrInvalidNodeReport = errors.New("invalid node report")
	// ErrNodeClaimed means another API key already reports under the name.
	ErrNodeClaimed = errors.New("another API key reports this node; forget the node in overload to move it to a new key")
	// ErrNodeReleased means an administrator released the node from the
	// reporting key, which must not take it back.
	ErrNodeReleased = errors.New("an administrator released this node from this API key; report it with a new key")
	// ErrTooManyNodes means the inventory, or the key's share of it, is full.
	ErrTooManyNodes = errors.New("too many nodes")
	// ErrNodeInPlacement means a placement still names the node.
	ErrNodeInPlacement = errors.New("node is in a placement")
)

// Online reports whether the node reported recently.
func (node Node) Online(now time.Time) bool { return now.Sub(node.LastSeen) <= NodeOfflineAfter }

var nodeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func invalidReport(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidNodeReport, reason) }

// Validate checks a report before it is stored.
func (report NodeReport) Validate() error {
	if !nodeName.MatchString(report.Name) {
		return invalidReport("invalid node name")
	}
	for _, text := range []string{report.OS, report.OSVersion, report.Chip, report.OverloadVersion, report.MLXVersion} {
		if len(text) > MaxNodeTextLength || strings.ContainsAny(text, "\r\n\x00") {
			return invalidReport("report fields must be short single lines")
		}
	}
	if report.MemoryBytes < 0 || report.DiskFreeBytes < 0 || report.LlamaBuild < 0 || len(report.Services) > MaxNodeServices || len(report.Downloads) > MaxNodeDownloads {
		return invalidReport("sizes must not be negative and lists must be short")
	}
	for _, service := range report.Services {
		if len(service.Model) > MaxNodeServiceModel || len(service.Endpoint) > MaxNodeEndpointLength || len(service.Health) > MaxNodeHealthLength {
			return invalidReport("invalid service")
		}
	}
	for _, download := range report.Downloads {
		if len(download) > MaxNodeDownloadLength {
			return invalidReport("invalid download")
		}
	}
	return nil
}

// Bounded trims a collected report to the limits Validate enforces.
func (report NodeReport) Bounded() NodeReport {
	line := func(text string, limit int) string {
		text = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(text)
		if len(text) > limit {
			text = strings.ToValidUTF8(text[:limit], "")
		}
		return text
	}
	report.OS = line(report.OS, MaxNodeTextLength)
	report.OSVersion = line(report.OSVersion, MaxNodeTextLength)
	report.Chip = line(report.Chip, MaxNodeTextLength)
	report.OverloadVersion = line(report.OverloadVersion, MaxNodeTextLength)
	report.MLXVersion = line(report.MLXVersion, MaxNodeTextLength)
	services := make([]NodeService, 0, min(len(report.Services), MaxNodeServices))
	for _, service := range report.Services[:min(len(report.Services), MaxNodeServices)] {
		services = append(services, NodeService{Model: line(service.Model, MaxNodeServiceModel), Endpoint: line(service.Endpoint, MaxNodeEndpointLength), Health: line(service.Health, MaxNodeHealthLength)})
	}
	report.Services = services
	downloads := make([]string, 0, min(len(report.Downloads), MaxNodeDownloads))
	for _, download := range report.Downloads[:min(len(report.Downloads), MaxNodeDownloads)] {
		downloads = append(downloads, line(download, MaxNodeDownloadLength))
	}
	report.Downloads = downloads
	return report
}

// VersionDrift is a runtime setting whose value differs between online
// nodes.
type VersionDrift struct {
	// Field is os_version, overload_version, llama_build or mlx_version.
	Field string
	// OS is set for os_version, which is compared only between nodes
	// running the same operating system.
	OS string
	// Nodes are the nodes that differ from the most common value, or every
	// node compared when no value is most common.
	Nodes []string
	// Even means no value is most common, so no node is singled out.
	Even bool
}

// DriftFields are the settings Drift compares, in display order.
var DriftFields = []string{"os_version", "overload_version", "llama_build", "mlx_version"}

// Drift lists, for each runtime setting, the online nodes whose value
// differs from the most common one, so a mixed fleet is easy to spot.
// Clusters need the same versions on every node.
func Drift(nodes []Node, now time.Time) []VersionDrift {
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
	var drift []VersionDrift
	for _, field := range DriftFields {
		read := values[field]
		groups := map[string][]Node{}
		for _, node := range nodes {
			if !node.Online(now) || read(node.Report) == "" {
				continue
			}
			// A Linux kernel release and a macOS version are not
			// comparable, so operating system versions are compared
			// per operating system.
			group := ""
			if field == "os_version" {
				group = node.Report.OS
			}
			groups[group] = append(groups[group], node)
		}
		for _, group := range slices.Sorted(maps.Keys(groups)) {
			if found, ok := driftIn(groups[group], read); ok {
				found.Field, found.OS = field, group
				drift = append(drift, found)
			}
		}
	}
	return drift
}

func driftIn(nodes []Node, read func(NodeReport) string) (VersionDrift, bool) {
	counts := map[string]int{}
	for _, node := range nodes {
		counts[read(node.Report)]++
	}
	if len(counts) < 2 {
		return VersionDrift{}, false
	}
	common, best, tied := "", 0, false
	for value, count := range counts {
		switch {
		case count > best:
			common, best, tied = value, count, false
		case count == best:
			tied = true
		}
	}
	var found VersionDrift
	found.Even = tied
	for _, node := range nodes {
		if tied || read(node.Report) != common {
			found.Nodes = append(found.Nodes, node.Report.Name)
		}
	}
	slices.Sort(found.Nodes)
	return found, true
}
