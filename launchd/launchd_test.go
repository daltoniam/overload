package launchd

import (
	"encoding/xml"
	"strings"
	"testing"
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
