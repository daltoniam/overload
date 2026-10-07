// Package launchd writes and controls per-user launchd agents on macOS.
package launchd

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Agent is a login service that launchd starts at login and restarts if it
// exits.
type Agent struct {
	Label string
	Args  []string
	Dir   string
	Log   string
	Env   map[string]string
}

func xmlEscape(value string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(value))
	return buf.String()
}

func (agent Agent) plist() []byte {
	var out bytes.Buffer
	out.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key := func(name, value string) {
		out.WriteString("\t<key>" + name + "</key>\n\t<string>" + xmlEscape(value) + "</string>\n")
	}
	key("Label", agent.Label)
	out.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range agent.Args {
		out.WriteString("\t\t<string>" + xmlEscape(arg) + "</string>\n")
	}
	out.WriteString("\t</array>\n")
	if len(agent.Env) > 0 {
		names := make([]string, 0, len(agent.Env))
		for name := range agent.Env {
			names = append(names, name)
		}
		sort.Strings(names)
		out.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, name := range names {
			out.WriteString("\t\t<key>" + xmlEscape(name) + "</key>\n\t\t<string>" + xmlEscape(agent.Env[name]) + "</string>\n")
		}
		out.WriteString("\t</dict>\n")
	}
	key("WorkingDirectory", agent.Dir)
	key("StandardOutPath", agent.Log)
	key("StandardErrorPath", agent.Log)
	out.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<true/>\n\t<key>ThrottleInterval</key>\n\t<integer>10</integer>\n\t<key>ProcessType</key>\n\t<string>Background</string>\n</dict>\n</plist>\n")
	return out.Bytes()
}

// Path is where the agent's plist lives.
func Path(label string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

// Write saves the agent's plist at Path(agent.Label).
func Write(agent Agent) error {
	return writeAt(Path(agent.Label), agent)
}

func writeAt(path string, agent Agent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, agent.plist(), 0o644)
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Start loads the agent saved for label.
func Start(label string) error {
	return launchctl("bootstrap", domain(), Path(label))
}

// Stop unloads the agent; an agent that is not loaded is not an error.
func Stop(label string) {
	_ = launchctl("bootout", domain()+"/"+label)
}

// Remove unloads the agent and deletes its plist.
func Remove(label string) error {
	Stop(label)
	if err := os.Remove(Path(label)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// State describes a loaded agent ("running (pid 123)") or reports "not
// loaded".
func State(label string) string {
	out, err := exec.Command("launchctl", "print", domain()+"/"+label).CombinedOutput()
	if err != nil {
		return "not loaded"
	}
	state, pid := "loaded", ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "state = "); ok && state == "loaded" {
			state = value
		}
		if value, ok := strings.CutPrefix(line, "pid = "); ok && pid == "" {
			pid = value
		}
	}
	if pid != "" {
		return state + " (pid " + pid + ")"
	}
	return state
}
