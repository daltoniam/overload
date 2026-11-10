package overload

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
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
	// Host and Port are where the first node serves the OpenAI-compatible
	// API. mlx_lm.server listens on 127.0.0.1 by default, which the
	// gateway on another machine cannot reach.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// Mode is tensor (faster decode) or pipeline (bigger models).
	Mode string `json:"mode,omitempty"`
}

// Placement modes.
const (
	PlacementTensor   = "tensor"
	PlacementPipeline = "pipeline"
)

// DefaultPlacementPort is mlx_lm.server's default port.
const DefaultPlacementPort = 8080

var (
	mlxModel  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	rdmaName  = regexp.MustCompile(`^rdma_[A-Za-z0-9]{1,16}$`)
	pythonBin = regexp.MustCompile(`^/[A-Za-z0-9._/+-]{1,200}$`)
)

// MaxPlacementNodes bounds a placement. The JACCL backend needs a cable
// between every pair of nodes and Thunderbolt has no switch, so a mesh
// larger than four needs more ports than a Mac has.
const MaxPlacementNodes = 4

// Validate checks a placement.
func (placement Placement) Validate() error {
	switch {
	case !settingName.MatchString(placement.Name):
		return errors.New("invalid placement name")
	case !mlxModel.MatchString(placement.Model):
		return errors.New("the model must be a Hugging Face repository such as mlx-community/Model-4bit")
	case len(placement.Nodes) < 2 || len(placement.Nodes) > MaxPlacementNodes:
		return fmt.Errorf("a placement needs 2 to %d nodes, each cabled to every other", MaxPlacementNodes)
	case net.ParseIP(placement.Rank0IP) == nil:
		return errors.New("rank0_ip must be an IP address of the first node")
	case placement.Host != "" && net.ParseIP(placement.Host) == nil:
		return errors.New("host must be an IP address the first node serves the API on")
	case placement.Port < 0 || placement.Port > 65535:
		return errors.New("port must be between 1 and 65535")
	case placement.Mode != "" && placement.Mode != PlacementTensor && placement.Mode != PlacementPipeline:
		return errors.New("mode must be tensor or pipeline")
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

// ServeHost is the address the first node serves the API on: Host, or
// Rank0IP when Host is unset.
func (placement Placement) ServeHost() string {
	if placement.Host != "" {
		return placement.Host
	}
	return placement.Rank0IP
}

// ServePort is the port the first node serves the API on.
func (placement Placement) ServePort() int {
	if placement.Port != 0 {
		return placement.Port
	}
	return DefaultPlacementPort
}

// BaseURL is the OpenAI-compatible endpoint the cluster serves once
// started, to save as a model in overload.
// A Host of 0.0.0.0 or :: binds every interface but is not an address
// anyone can connect to, so the first node's Rank0IP is advertised instead.
func (placement Placement) BaseURL() string {
	host := placement.ServeHost()
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = placement.Rank0IP
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(placement.ServePort())) + "/v1"
}

// Hostfile is the JSON hostfile for mlx.launch --backend jaccl.
func (placement Placement) Hostfile() ([]byte, error) {
	if err := placement.Validate(); err != nil {
		return nil, err
	}
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
	command := []string{"mlx.launch", "--backend", "jaccl", "--hostfile", hostfile, "--", python, "-m", "mlx_lm.server", "--model", placement.Model, "--host", placement.ServeHost(), "--port", strconv.Itoa(placement.ServePort())}
	if placement.Mode == PlacementPipeline {
		command = append(command, "--pipeline")
	}
	quoted := make([]string, len(command))
	for index, arg := range command {
		quoted[index] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// shellQuote quotes arg for a POSIX shell, leaving simple words bare so
// the usual command stays readable.
func shellQuote(arg string) string {
	if shellSafe.MatchString(arg) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}
