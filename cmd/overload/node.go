package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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

const nodeUsage = "usage: overload node report --server URL [--every 1m] | install --server URL | uninstall"

func manageNode(args []string) error {
	if len(args) == 0 {
		return errors.New(nodeUsage)
	}
	flags := flag.NewFlagSet("node "+args[0], flag.ContinueOnError)
	server := flags.String("server", "", "overload address, such as https://overload.example.com")
	every := flags.Duration("every", 0, "keep reporting at this interval (default: report once)")
	name := flags.String("name", "", "node name (default: the host name)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New(nodeUsage)
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
			err := sendReport(ctx, http.DefaultClient, target, key, collectReport(ctx, *name))
			if *every <= 0 {
				return err
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "node report failed:", err)
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
		agent := launchd.Agent{Label: label + ".node", Args: []string{executable, "node", "report", "--server", *server, "--every", "1m"}, Dir: filepath.Dir(configPath()), Log: filepath.Join(logs, "node.log"), Env: map[string]string{"OVERLOAD_CONFIG": configPath()}}
		launchd.Stop(agent.Label)
		if err := launchd.Write(agent); err != nil {
			return err
		}
		if err := launchd.Start(agent.Label); err != nil {
			return err
		}
		fmt.Printf("Reporting to %s every minute (launchd agent %s)\n", *server, agent.Label)
		return nil
	case "uninstall":
		return launchd.Remove(label + ".node")
	default:
		return errors.New(nodeUsage)
	}
}

func nodeEndpoint(server string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", errors.New("--server must be overload's address, such as https://overload.example.com")
	}
	return parsed.String() + "/api/nodes", nil
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

// commandOutput runs a short command and returns its trimmed output, or ""
// when it is not available.
func commandOutput(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// collectReport gathers what this machine can say about itself; anything
// unavailable is left empty.
func collectReport(ctx context.Context, name string) overload.NodeReport {
	if name == "" {
		name, _ = os.Hostname()
		name = strings.TrimSuffix(name, ".local")
	}
	report := overload.NodeReport{Name: name, OS: runtime.GOOS, OverloadVersion: version}
	if runtime.GOOS == "darwin" {
		report.OSVersion = commandOutput(ctx, "sw_vers", "-productVersion")
		report.Chip = commandOutput(ctx, "sysctl", "-n", "machdep.cpu.brand_string")
		report.MemoryBytes, _ = strconv.ParseInt(commandOutput(ctx, "sysctl", "-n", "hw.memsize"), 10, 64)
	} else {
		report.OSVersion = commandOutput(ctx, "uname", "-r")
	}
	if binary, err := llamaServerBinary(""); err == nil {
		if detected, err := localmodels.DetectLlama(ctx, binary); err == nil {
			report.LlamaBuild = detected.Build
		}
	}
	report.MLXVersion = commandOutput(ctx, "python3", "-c", "import mlx.core as mx; print(mx.__version__)")
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
	return report
}
