// Package tunnel exposes overload's GitHub webhook to the internet through a
// Cloudflare Tunnel on macOS. It drives the cloudflared command, writes a
// tunnel configuration that forwards only the webhook path, and runs
// cloudflared as a launchd login service next to overload.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/daltoniam/overload/launchd"
)

// WebhookPath is the only path the tunnel forwards.
const WebhookPath = "/webhooks/github"

var (
	hostnamePattern = regexp.MustCompile(`^(?i)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
	namePattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	loginURL        = regexp.MustCompile(`https://dash\.cloudflare\.com/argotunnel\S*`)
)

// Runner runs a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Service controls the launchd agent that runs cloudflared.
type Service interface {
	Install(agent launchd.Agent) error
	Remove(label string) error
	State(label string) string
}

type launchdService struct{}

func (launchdService) Install(agent launchd.Agent) error {
	launchd.Stop(agent.Label)
	if err := launchd.Write(agent); err != nil {
		return err
	}
	return launchd.Start(agent.Label)
}

func (launchdService) Remove(label string) error { return launchd.Remove(label) }
func (launchdService) State(label string) string { return launchd.State(label) }

// Manager sets up and reports on the tunnel for one overload install.
type Manager struct {
	// Home is the overload install directory; tunnel files live in
	// Home/cloudflared.
	Home string
	// Label is the install's launchd label prefix; the tunnel runs as
	// Label + ".tunnel".
	Label string
	// LocalURL is where overload listens, for example http://127.0.0.1:8082.
	LocalURL string
	// OnReady is called with the public webhook URL after the tunnel works.
	// A non-empty message is shown as a step.
	OnReady func(ctx context.Context, webhookURL string) (string, error)

	Run       Runner
	Service   Service
	Probe     func(ctx context.Context, url string) (int, error)
	LookPath  func(string) (string, error)
	Supported bool
	// CertPath is cloudflared's login certificate.
	CertPath string

	mu  sync.Mutex
	job Job
}

// NewManager returns a Manager that uses the real cloudflared and launchd.
func NewManager(home, label, localURL string) *Manager {
	userHome, _ := os.UserHomeDir()
	return &Manager{
		Home:      home,
		Label:     label,
		LocalURL:  strings.TrimRight(localURL, "/"),
		Run:       runCommand,
		Service:   launchdService{},
		Probe:     probe,
		LookPath:  lookCloudflared,
		Supported: runtime.GOOS == "darwin",
		CertPath:  filepath.Join(userHome, ".cloudflared", "cert.pem"),
	}
}

// Job is the progress of the last login or setup.
type Job struct {
	Kind     string
	Running  bool
	Steps    []string
	Err      string
	LoginURL string
	Finished time.Time
}

// Config is the tunnel this install runs.
type Config struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
}

// WebhookURL is the public URL GitHub should call.
func (config Config) WebhookURL() string {
	if config.Hostname == "" {
		return ""
	}
	return "https://" + config.Hostname + WebhookPath
}

// Status is what the UI shows.
type Status struct {
	Supported   bool
	Cloudflared string
	LoggedIn    bool
	Config      Config
	Service     string
	Job         Job
}

func (manager *Manager) dir() string          { return filepath.Join(manager.Home, "cloudflared") }
func (manager *Manager) configFile() string   { return filepath.Join(manager.dir(), "config.yml") }
func (manager *Manager) stateFile() string    { return filepath.Join(manager.dir(), "tunnel.json") }
func (manager *Manager) serviceLabel() string { return manager.Label + ".tunnel" }
func (manager *Manager) credentials(id string) string {
	return filepath.Join(manager.dir(), id+".json")
}

// Status reports cloudflared, login, the saved tunnel and its service.
func (manager *Manager) Status() Status {
	manager.mu.Lock()
	job := manager.job
	job.Steps = append([]string(nil), job.Steps...)
	manager.mu.Unlock()
	status := Status{Supported: manager.Supported, Job: job}
	if !manager.Supported {
		return status
	}
	status.Cloudflared, _ = manager.LookPath("cloudflared")
	_, err := os.Stat(manager.CertPath)
	status.LoggedIn = err == nil
	status.Config, _ = manager.saved()
	if status.Config.ID != "" {
		status.Service = manager.Service.State(manager.serviceLabel())
	}
	return status
}

func (manager *Manager) saved() (Config, error) {
	var config Config
	data, err := os.ReadFile(manager.stateFile())
	if err != nil {
		return config, err
	}
	return config, json.Unmarshal(data, &config)
}

// begin starts a job unless one is running.
func (manager *Manager) begin(kind string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.job.Running {
		return errors.New("another Cloudflare step is still running")
	}
	manager.job = Job{Kind: kind, Running: true}
	return nil
}

func (manager *Manager) step(format string, args ...any) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.job.Steps = append(manager.job.Steps, fmt.Sprintf(format, args...))
}

func (manager *Manager) finish(err error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.job.Running = false
	manager.job.Finished = time.Now()
	if err != nil {
		manager.job.Err = err.Error()
	}
}

// Login starts `cloudflared tunnel login`, which opens the browser so the
// user can authorize a Cloudflare domain. It returns at once; Status reports
// progress and the login link in case the browser did not open.
func (manager *Manager) Login() error {
	if !manager.Supported {
		return errors.New("one-click tunnel setup runs on macOS; see the Cloudflare Tunnel guide")
	}
	cloudflared, err := manager.LookPath("cloudflared")
	if err != nil {
		return err
	}
	if err := manager.begin("login"); err != nil {
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		manager.step("Opening Cloudflare in your browser; choose the domain for the webhook.")
		output, err := manager.Run(ctx, cloudflared, "tunnel", "login")
		if link := loginURL.FindString(string(output)); link != "" {
			manager.mu.Lock()
			manager.job.LoginURL = link
			manager.mu.Unlock()
		}
		if err == nil {
			manager.step("Logged in to Cloudflare.")
		}
		manager.finish(commandError("cloudflared tunnel login", output, err))
	}()
	return nil
}

// Setup creates (or reuses) the named tunnel, points hostname at it, writes
// a configuration that forwards only the webhook path, runs it as a login
// service and checks it from the internet. It returns at once; Status
// reports progress.
func (manager *Manager) Setup(name, hostname string) error {
	if !manager.Supported {
		return errors.New("one-click tunnel setup runs on macOS; see the Cloudflare Tunnel guide")
	}
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if !hostnamePattern.MatchString(hostname) || len(hostname) > 253 {
		return errors.New("enter a hostname such as overload.example.com")
	}
	if !namePattern.MatchString(name) {
		return errors.New("tunnel names use letters, digits, - and _")
	}
	cloudflared, err := manager.LookPath("cloudflared")
	if err != nil {
		return err
	}
	if _, err := os.Stat(manager.CertPath); err != nil {
		return errors.New("log in to Cloudflare first")
	}
	if err := manager.begin("setup"); err != nil {
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		manager.finish(manager.setup(ctx, cloudflared, name, hostname))
	}()
	return nil
}

func (manager *Manager) setup(ctx context.Context, cloudflared, name, hostname string) error {
	if err := os.MkdirAll(manager.dir(), 0o700); err != nil {
		return err
	}
	id, err := manager.tunnelID(ctx, cloudflared, name)
	if err != nil {
		return err
	}
	if id == "" {
		output, err := manager.Run(ctx, cloudflared, "tunnel", "create", "--output", "json", "--credentials-file", manager.credentials("new"), name)
		if err != nil {
			return commandError("cloudflared tunnel create", output, err)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(jsonPart(output), &created); err != nil || created.ID == "" {
			return fmt.Errorf("could not read the new tunnel's ID: %s", strings.TrimSpace(string(output)))
		}
		id = created.ID
		if err := os.Rename(manager.credentials("new"), manager.credentials(id)); err != nil {
			return err
		}
		manager.step("Created tunnel %s.", name)
	} else {
		if err := manager.findCredentials(id); err != nil {
			return err
		}
		manager.step("Using the existing tunnel %s.", name)
	}
	output, err := manager.Run(ctx, cloudflared, "tunnel", "route", "dns", id, hostname)
	if err != nil {
		return commandError("cloudflared tunnel route dns", output, err)
	}
	manager.step("Pointed %s at the tunnel.", hostname)
	config := Config{Name: name, ID: id, Hostname: hostname}
	if err := manager.writeConfig(config); err != nil {
		return err
	}
	manager.step("Wrote %s; only %s is forwarded.", manager.configFile(), WebhookPath)
	agent := launchd.Agent{
		Label: manager.serviceLabel(),
		Args:  []string{cloudflared, "--no-autoupdate", "--config", manager.configFile(), "tunnel", "run", id},
		Dir:   manager.dir(),
		Log:   filepath.Join(manager.Home, "logs", "tunnel.log"),
	}
	if err := os.MkdirAll(filepath.Dir(agent.Log), 0o700); err != nil {
		return err
	}
	if err := manager.Service.Install(agent); err != nil {
		return err
	}
	manager.step("Started the tunnel as a login service (%s).", agent.Label)
	if err := manager.verify(ctx, config); err != nil {
		return err
	}
	manager.step("GitHub can reach %s.", config.WebhookURL())
	if manager.OnReady != nil {
		message, err := manager.OnReady(ctx, config.WebhookURL())
		if err != nil {
			return fmt.Errorf("the tunnel works, but updating the GitHub App's webhook URL failed: %w", err)
		}
		if message != "" {
			manager.step("%s", message)
		}
	}
	return nil
}

// tunnelID returns the ID of the tunnel with name, or "" if none exists.
func (manager *Manager) tunnelID(ctx context.Context, cloudflared, name string) (string, error) {
	output, err := manager.Run(ctx, cloudflared, "tunnel", "list", "--output", "json", "--name", name)
	if err != nil {
		return "", commandError("cloudflared tunnel list", output, err)
	}
	var tunnels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(jsonPart(output), &tunnels); err != nil {
		return "", fmt.Errorf("could not read the tunnel list: %w", err)
	}
	for _, tunnel := range tunnels {
		if tunnel.Name == name {
			return tunnel.ID, nil
		}
	}
	return "", nil
}

// findCredentials copies an existing tunnel's credentials into the install,
// from ~/.cloudflared where `cloudflared tunnel create` writes them.
func (manager *Manager) findCredentials(id string) error {
	if _, err := os.Stat(manager.credentials(id)); err == nil {
		return nil
	}
	source := filepath.Join(filepath.Dir(manager.CertPath), id+".json")
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("a tunnel with this name exists, but its credentials are not on this Mac (looked for %s); choose another tunnel name", source)
	}
	return os.WriteFile(manager.credentials(id), data, 0o600)
}

func (manager *Manager) writeConfig(config Config) error {
	yaml := fmt.Sprintf(`# Written by overload. Only the GitHub webhook is forwarded; every other
# path gets a 404 from Cloudflare and never reaches this Mac.
tunnel: %s
credentials-file: %s
ingress:
  - hostname: %s
    path: ^%s$
    service: %s
  - service: http_status:404
`, config.ID, manager.credentials(config.ID), config.Hostname, regexp.QuoteMeta(WebhookPath), manager.LocalURL)
	if err := os.WriteFile(manager.configFile(), []byte(yaml), 0o600); err != nil {
		return err
	}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(manager.stateFile(), data, 0o600)
}

// verify waits until an unsigned POST to the public webhook reaches overload
// (which rejects it with 401) and the UI is not reachable.
func (manager *Manager) verify(ctx context.Context, config Config) error {
	var last string
	for attempt := 0; attempt < 30; attempt++ {
		code, err := manager.Probe(ctx, config.WebhookURL())
		if err == nil && code == http.StatusUnauthorized {
			if code, err := manager.Probe(ctx, "https://"+config.Hostname+"/"); err == nil && code != http.StatusNotFound {
				return fmt.Errorf("https://%s/ answered %d; only the webhook should be reachable", config.Hostname, code)
			}
			return nil
		}
		last = fmt.Sprintf("status %d", code)
		if err != nil {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("the tunnel is running but %s is not reachable yet (%s); DNS can take a few minutes, check again with Verify", config.WebhookURL(), last)
}

// Verify rechecks the saved tunnel from the internet.
func (manager *Manager) Verify(ctx context.Context) error {
	config, err := manager.saved()
	if err != nil {
		return errors.New("no tunnel is set up")
	}
	return manager.verify(ctx, config)
}

// Stop removes the login service. The tunnel and its DNS record stay in
// Cloudflare so Setup can start it again.
func (manager *Manager) Stop() error {
	if !manager.Supported {
		return errors.New("tunnel services are managed on macOS only")
	}
	return manager.Service.Remove(manager.serviceLabel())
}

func commandError(command string, output []byte, err error) error {
	if err == nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	detail := strings.TrimSpace(lines[len(lines)-1])
	if len(detail) > 300 {
		detail = detail[:300]
	}
	return fmt.Errorf("%s failed: %v: %s", command, err, detail)
}

// jsonPart drops log lines cloudflared prints before its JSON output.
func jsonPart(output []byte) []byte {
	text := string(output)
	if start := strings.IndexAny(text, "[{"); start >= 0 {
		return []byte(text[start:])
	}
	return output
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func lookCloudflared(name string) (string, error) {
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", errors.New("cloudflared is not installed; run `brew install cloudflared`")
	}
	return path, nil
}

func probe(ctx context.Context, url string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	method := http.MethodGet
	if strings.HasSuffix(url, WebhookPath) {
		method = http.MethodPost
	}
	request, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader("{}"))
	if err != nil {
		return 0, err
	}
	request.Header.Set("User-Agent", "overload-tunnel-check")
	response, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if err != nil {
		return 0, err
	}
	_ = response.Body.Close()
	return response.StatusCode, nil
}
