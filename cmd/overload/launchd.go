package main

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"sort"
)

type launchAgent struct {
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

func (agent launchAgent) plist() []byte {
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

func writePlist(path string, agent launchAgent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, agent.plist(), 0o644)
}
