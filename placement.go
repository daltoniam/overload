package overload

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// Placement records a model sharded across Macs with MLX's distributed
// launcher over RDMA on Thunderbolt 5 (the JACCL backend). Overload keeps
// the placement and generates the launcher's hostfile and command; it does
// not start the cluster itself.
type Placement struct {
	Name  string `json:"name"`
	Model string `json:"model"`
	// Nodes are the reporting nodes in rank order; their names must be
	// reachable over SSH from the machine that runs the launcher.
	Nodes []string `json:"nodes"`
	// Rank0IP is an address of the first node every node can reach.
	Rank0IP string `json:"rank0_ip"`
	// RDMA[i][j] is node i's RDMA device that reaches node j; the
	// diagonal is empty.
	RDMA [][]string `json:"rdma"`
	// Python is the interpreter path, the same on every node.
	Python string `json:"python,omitempty"`
}

var (
	mlxModel  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	rdmaName  = regexp.MustCompile(`^rdma_[A-Za-z0-9]{1,16}$`)
	pythonBin = regexp.MustCompile(`^/[A-Za-z0-9._/+-]{1,200}$`)
)

// MaxPlacementNodes bounds a placement; Thunderbolt meshes are small.
const MaxPlacementNodes = 8

// Validate checks a placement.
func (placement Placement) Validate() error {
	switch {
	case !settingName.MatchString(placement.Name):
		return errors.New("invalid placement name")
	case !mlxModel.MatchString(placement.Model):
		return errors.New("the model must be a Hugging Face repository such as mlx-community/Model-4bit")
	case len(placement.Nodes) < 2 || len(placement.Nodes) > MaxPlacementNodes:
		return fmt.Errorf("a placement needs 2 to %d nodes", MaxPlacementNodes)
	case net.ParseIP(placement.Rank0IP) == nil:
		return errors.New("rank0_ip must be an IP address of the first node")
	case placement.Python != "" && !pythonBin.MatchString(placement.Python):
		return errors.New("python must be an absolute path")
	case len(placement.RDMA) != len(placement.Nodes):
		return errors.New("rdma needs one row per node")
	}
	seen := map[string]bool{}
	for _, node := range placement.Nodes {
		if !nodeName.MatchString(node) || seen[node] {
			return fmt.Errorf("invalid or repeated node %q", node)
		}
		seen[node] = true
	}
	for row, devices := range placement.RDMA {
		if len(devices) != len(placement.Nodes) {
			return fmt.Errorf("rdma row %d needs one entry per node", row)
		}
		for column, device := range devices {
			if (row == column) != (device == "") || (device != "" && !rdmaName.MatchString(device)) {
				return fmt.Errorf("rdma[%d][%d] must be empty for the node itself and an rdma_ device otherwise", row, column)
			}
		}
	}
	return nil
}

// Hostfile is the JSON hostfile for mlx.launch --backend jaccl.
func (placement Placement) Hostfile() ([]byte, error) {
	type host struct {
		SSH  string    `json:"ssh"`
		IPs  []string  `json:"ips"`
		RDMA []*string `json:"rdma"`
	}
	hosts := make([]host, len(placement.Nodes))
	for index, node := range placement.Nodes {
		hosts[index] = host{SSH: node, IPs: []string{}}
		if index == 0 {
			hosts[index].IPs = []string{placement.Rank0IP}
		}
		for _, device := range placement.RDMA[index] {
			if device == "" {
				hosts[index].RDMA = append(hosts[index].RDMA, nil)
				continue
			}
			value := device
			hosts[index].RDMA = append(hosts[index].RDMA, &value)
		}
	}
	return json.MarshalIndent(hosts, "", "  ")
}

// Command is the launch command, run from a machine with SSH access to
// every node, with the hostfile saved as hostfile.
func (placement Placement) Command(hostfile string) string {
	python := placement.Python
	if python == "" {
		python = "python3"
	}
	return strings.Join([]string{"mlx.launch", "--backend", "jaccl", "--hostfile", hostfile, "--", python, "-m", "mlx_lm.server", "--model", placement.Model}, " ")
}
