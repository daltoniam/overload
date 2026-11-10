package launchd

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLaunchAgentPlistIsValidXML(t *testing.T) {
	agent := Agent{Label: "dev.overload", Args: []string{"/opt/homebrew/bin/overload", "serve"}, Dir: "/Users/me/Library/Application Support/overload", Log: "/tmp/a&b.log", Env: map[string]string{"OVERLOAD_CONFIG": "/x/overload.env", "PATH": "/usr/bin"}}
	data := agent.plist()
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	decoder.Strict = false
	for {
		if _, err := decoder.Token(); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("invalid XML: %v\n%s", err, data)
		}
	}
	for _, want := range []string{"<string>dev.overload</string>", "<string>serve</string>", "a&amp;b.log", "<key>OVERLOAD_CONFIG</key>", "<key>KeepAlive</key>"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestLaunchAgentExitTimeout(t *testing.T) {
	agent := Agent{Label: "dev.overload", Args: []string{"/bin/true"}}
	if strings.Contains(string(agent.plist()), "ExitTimeOut") {
		t.Error("an agent without an exit timeout should keep launchd's default")
	}
	agent.ExitTimeout = 30 * time.Second
	if !strings.Contains(string(agent.plist()), "<key>ExitTimeOut</key>\n\t<integer>30</integer>") {
		t.Errorf("plist missing a 30 second ExitTimeOut:\n%s", agent.plist())
	}
	for _, timeout := range []time.Duration{time.Millisecond, 500 * time.Millisecond, time.Second} {
		agent.ExitTimeout = timeout
		if !strings.Contains(string(agent.plist()), "<key>ExitTimeOut</key>\n\t<integer>1</integer>") {
			t.Errorf("an ExitTimeout of %s should round up to 1 second, not 0 (no limit):\n%s", timeout, agent.plist())
		}
	}
	agent.ExitTimeout = 1500 * time.Millisecond
	if !strings.Contains(string(agent.plist()), "<key>ExitTimeOut</key>\n\t<integer>2</integer>") {
		t.Errorf("an ExitTimeout of 1.5s should round up to 2 seconds:\n%s", agent.plist())
	}
}

func TestWriteReplacesTheAgentAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev.overload.plist")
	for _, label := range []string{"first", "second"} {
		if err := writeAt(path, Agent{Label: label, Args: []string{"/bin/true"}}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "<string>second</string>") {
		t.Fatalf("agent = %s, %v", data, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
}
