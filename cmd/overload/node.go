package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/launchd"
	"github.com/daltoniam/overload/localmodels"
)

const nodeUsage = "usage: overload node report --server URL [--every 1m] [--name NAME] [--python PATH] | install --server URL [--every 1m] [--name NAME] [--python PATH] | uninstall"

// minReportInterval keeps a misconfigured node from hammering overload.
const minReportInterval = 10 * time.Second

func manageNode(args []string) error {
	if len(args) == 0 {
		return errors.New(nodeUsage)
	}
	flags := flag.NewFlagSet("node "+args[0], flag.ContinueOnError)
	server := flags.String("server", "", "overload address, such as https://overload.example.com")
	every := flags.Duration("every", 0, "keep reporting at this interval, at least 10s (default: report once; install defaults to 1m)")
	name := flags.String("name", "", "node name (default: the host name)")
	python := flags.String("python", os.Getenv("OVERLOAD_PYTHON"), "Python with MLX installed, such as a virtual environment's bin/python (default: OVERLOAD_PYTHON, else python3 on PATH)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New(nodeUsage)
	}
	if *every != 0 && *every < minReportInterval {
		return fmt.Errorf("--every must be at least %s", minReportInterval)
	}
	if *name != "" {
		if err := (overload.NodeReport{Name: *name}).Validate(); err != nil {
			return errors.New("--name must be letters, digits, dots, dashes or underscores, at most 64")
		}
	} else if fallback := defaultNodeName(); args[0] != "uninstall" && (overload.NodeReport{Name: fallback}).Validate() != nil {
		return fmt.Errorf("this machine's host name %q is not a valid node name (letters, digits, dots, dashes or underscores, at most 64); pass --name", fallback)
	}
	if *python != "" && !filepath.IsAbs(*python) {
		return errors.New("--python must be an absolute path")
	}
	label := os.Getenv("OVERLOAD_INSTALL_LABEL")
	if label == "" {
		label = defaultLabel
	}
	switch args[0] {
	case "report":
		target, err := nodeEndpoint(*server)
		if err != nil {
			return err
		}
		key := os.Getenv("OVERLOAD_NODE_KEY")
		if key == "" {
			return errors.New("set OVERLOAD_NODE_KEY to an API key with the nodes scope (in the install's configuration file for the service)")
		}
		ctx := context.Background()
		for {
			err := sendReport(ctx, http.DefaultClient, target, key, collectReport(ctx, systemProbe, *name, *python))
			if *every <= 0 {
				return err
			}
			if err != nil {
				slog.Warn("node report failed", "server", target, "error", err)
			}
			time.Sleep(*every)
		}
	case "install":
		if runtime.GOOS != "darwin" {
			return errors.New("node install uses launchd; on Linux run overload node report --every 1m under systemd")
		}
		if _, err := nodeEndpoint(*server); err != nil {
			return err
		}
		if os.Getenv("OVERLOAD_NODE_KEY") == "" {
			return fmt.Errorf("add OVERLOAD_NODE_KEY=<an API key with the nodes scope> to %s first; the service reads it from there", configPath())
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		logs := filepath.Join(filepath.Dir(configPath()), "logs")
		if err := os.MkdirAll(logs, 0o700); err != nil {
			return err
		}
		interval := *every
		if interval == 0 {
			interval = time.Minute
		}
		agent := nodeAgent(label, executable, *server, *name, *python, logs, interval)
		launchd.Stop(agent.Label)
		if err := launchd.Write(agent); err != nil {
			return err
		}
		if err := launchd.Start(agent.Label); err != nil {
			return err
		}
		fmt.Printf("Reporting to %s every %s (launchd agent %s)\n", *server, interval, agent.Label)
		return nil
	case "uninstall":
		return launchd.Remove(label + ".node")
	default:
		return errors.New(nodeUsage)
	}
}

// nodeAgent is the launchd agent that reports at every. launchd starts
// agents with a minimal PATH, so it gets the same PATH as overload's own
// service to find Homebrew's llama-server and python3.
func nodeAgent(label, executable, server, name, python, logs string, every time.Duration) launchd.Agent {
	args := []string{executable, "node", "report", "--server", server, "--every", every.String()}
	if name != "" {
		args = append(args, "--name", name)
	}
	if python != "" {
		args = append(args, "--python", python)
	}
	return launchd.Agent{Label: label + ".node", Args: args, Dir: filepath.Dir(configPath()), Log: filepath.Join(logs, "node.log"), Env: map[string]string{"OVERLOAD_CONFIG": configPath(), "PATH": servicePath}}
}

// nodeEndpoint is the report address on server. Plain http is allowed only
// to this machine, since the request carries an API key.
func nodeEndpoint(server string) (string, error) {
	invalid := errors.New("--server must be overload's address, such as https://overload.example.com")
	parsed, err := url.Parse(server)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", invalid
	}
	if parsed.Scheme == "http" && !loopbackHost(parsed.Hostname()) {
		return "", errors.New("--server must use https unless overload runs on this machine; plain http would send the API key unencrypted")
	}
	return url.JoinPath(parsed.String(), "api", "nodes")
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sendReport(ctx context.Context, client *http.Client, target, key string, report overload.NodeReport) error {
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("overload answered %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	return nil
}

// nodeProbe runs the commands a report needs; tests replace it so they
// never run sysctl, sw_vers or python3.
type nodeProbe struct {
	run      func(ctx context.Context, name string, args ...string) (string, error)
	lookPath func(string) (string, error)
	goos     string
}

var systemProbe = nodeProbe{run: runCommand, lookPath: exec.LookPath, goos: runtime.GOOS}

// runCommand runs a short command and returns its trimmed output.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// output is a command's output, or "" when it is not available.
func (probe nodeProbe) output(ctx context.Context, name string, args ...string) string {
	out, err := probe.run(ctx, name, args...)
	if err != nil {
		return ""
	}
	return out
}

// mlxPython is the interpreter to ask for MLX's version, or "" when there is
// none worth asking. Without the Command Line Tools, macOS's /usr/bin/python3
// is a stub that opens an install dialog each time it runs, which on a
// headless node would be every minute.
func (probe nodeProbe) mlxPython(ctx context.Context, configured string) string {
	if configured != "" {
		return configured
	}
	found, err := probe.lookPath("python3")
	if err != nil {
		return ""
	}
	if probe.goos == "darwin" && found == "/usr/bin/python3" {
		if _, err := probe.run(ctx, "xcode-select", "-p"); err != nil {
			return ""
		}
	}
	return found
}

// hostname is os.Hostname, replaced in tests.
var hostname = os.Hostname

// defaultNodeName is the host name without .local, which is also the name
// SSH usually reaches the machine by.
func defaultNodeName() string {
	name, _ := hostname()
	return strings.TrimSuffix(name, ".local")
}

// collectReport gathers what this machine can say about itself; anything
// unavailable is left empty, and the result fits the server's limits.
func collectReport(ctx context.Context, probe nodeProbe, name, python string) overload.NodeReport {
	if name == "" {
		name = defaultNodeName()
	}
	report := overload.NodeReport{Name: name, OS: probe.goos, OverloadVersion: version}
	if probe.goos == "darwin" {
		report.OSVersion = probe.output(ctx, "sw_vers", "-productVersion")
		report.Chip = probe.output(ctx, "sysctl", "-n", "machdep.cpu.brand_string")
		report.MemoryBytes, _ = strconv.ParseInt(probe.output(ctx, "sysctl", "-n", "hw.memsize"), 10, 64)
	} else {
		report.OSVersion = probe.output(ctx, "uname", "-r")
	}
	binary := os.Getenv("OVERLOAD_LLAMA_SERVER")
	if binary == "" {
		binary, _ = probe.lookPath("llama-server")
	}
	if binary != "" {
		if detected, err := localmodels.DetectLlama(ctx, binary); err == nil {
			report.LlamaBuild = detected.Build
		}
	}
	if interpreter := probe.mlxPython(ctx, python); interpreter != "" {
		report.MLXVersion = probe.output(ctx, interpreter, "-c", "import mlx.core as mx; print(mx.__version__)")
	}
	if dir, err := modelsDir(""); err == nil {
		store := localmodels.NewStore(dir)
		if installed, err := store.List(); err == nil {
			for _, entry := range installed {
				report.Downloads = append(report.Downloads, entry.ID+" "+entry.Quant)
			}
		}
		if services, err := store.Services(); err == nil {
			client := &http.Client{Timeout: 3 * time.Second}
			for _, service := range services {
				report.Services = append(report.Services, overload.NodeService{Model: service.ID, Endpoint: service.BaseURL(), Health: string(localmodels.CheckHealth(ctx, client, service))})
			}
		}
		if _, free, err := store.Usage(); err == nil {
			report.DiskFreeBytes = free
		}
	}
	return report.Bounded()
}
