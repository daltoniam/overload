package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/daltoniam/overload/postgres"
	"github.com/jackc/pgx/v5"
)

const (
	defaultLabel      = "dev.overload"
	defaultUIPort     = 8082
	firstPostgresPort = 55432
)

var configOrder = []string{"DATABASE_URL", "OVERLOAD_ADDR", "OVERLOAD_BASE_URL", "OVERLOAD_UI_USER", "OVERLOAD_UI_PASSWORD", "OVERLOAD_POSTGRES", "OVERLOAD_POSTGRES_DATA", "OVERLOAD_PG_PORT", "OVERLOAD_POSTGRES_BIN", "OVERLOAD_INSTALL_LABEL"}

type installation struct {
	home   string
	label  string
	config string
	values map[string]string
}

func installFlags(name string) (*flag.FlagSet, *string, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	home := flags.String("home", "", "install directory (default: user config dir/overload)")
	label := flags.String("label", defaultLabel, "launchd label prefix")
	return flags, home, label
}

var validLabel = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)

func openInstallation(home, label string) (*installation, error) {
	if !validLabel.MatchString(label) {
		return nil, errors.New("--label must be reverse-DNS, for example dev.overload")
	}
	if home == "" {
		var err error
		if home, err = defaultHome(); err != nil {
			return nil, err
		}
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	inst := &installation{home: home, label: label, config: filepath.Join(home, configFileName), values: map[string]string{}}
	values, err := readEnvFile(inst.config)
	if err == nil {
		inst.values = values
		if saved := values["OVERLOAD_INSTALL_LABEL"]; saved != "" {
			inst.label = saved
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return inst, nil
}

func install(args []string) error {
	flags, home, label := installFlags("install")
	port := flags.Int("port", defaultUIPort, "UI port on 127.0.0.1")
	databaseMode := flags.String("database", "auto", "auto, native (Homebrew Postgres), or url")
	databaseURL := flags.String("database-url", "", "existing Postgres URL (implies --database url)")
	postgresBin := flags.String("postgres-bin", "", "directory containing initdb and postgres")
	binary := flags.String("binary", "", "overload binary the service runs (default: this one)")
	noStart := flags.Bool("no-start", false, "write configuration without starting services")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: overload install [--port N] [--database auto|native|url] [--database-url URL]")
	}
	inst, err := openInstallation(*home, *label)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(inst.home, "logs"), 0o700); err != nil {
		return err
	}
	exe, err := serviceBinary(*binary)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fresh := len(inst.values) == 0
	if fresh {
		fmt.Printf("Installing overload into %s\n", inst.home)
		if err := inst.configure(*port, *databaseMode, *databaseURL, *postgresBin); err != nil {
			return err
		}
	} else {
		fmt.Printf("Existing install found at %s; keeping its configuration\n", inst.home)
	}
	if !*noStart && runtime.GOOS == "darwin" {
		inst.stopServices()
		if err := waitFree(inst.values["OVERLOAD_ADDR"], inst.values["OVERLOAD_PG_PORT"]); err != nil {
			return err
		}
	}
	addr := inst.values["OVERLOAD_ADDR"]
	if busy(addr) {
		return fmt.Errorf("%s is already in use; rerun with --port", addr)
	}
	if err := inst.preparePostgres(ctx); err != nil {
		return err
	}
	if err := writeEnvFile(inst.config, inst.values, configOrder); err != nil {
		return err
	}
	if err := inst.migrate(ctx); err != nil {
		return err
	}
	if *noStart {
		fmt.Printf("Configuration written to %s. Start with: OVERLOAD_CONFIG=%s %s serve\n", inst.config, inst.config, exe)
		return nil
	}
	if runtime.GOOS != "darwin" {
		fmt.Printf("Services are only managed on macOS. Start overload with:\n  OVERLOAD_CONFIG=%s %s serve\n", inst.config, exe)
		return nil
	}
	if err := inst.startServices(exe); err != nil {
		return err
	}
	base := inst.values["OVERLOAD_BASE_URL"]
	if err := waitHealthy(ctx, base+"/healthz", 90*time.Second); err != nil {
		return fmt.Errorf("overload did not become healthy (logs in %s): %w", filepath.Join(inst.home, "logs"), err)
	}
	fmt.Printf("\noverload is running at %s\n  user:     %s\n  password: %s\n  config:   %s (mode 600)\n  logs:     %s\n", base, inst.values["OVERLOAD_UI_USER"], inst.values["OVERLOAD_UI_PASSWORD"], inst.config, filepath.Join(inst.home, "logs"))
	for _, server := range detectLocalModels() {
		fmt.Printf("\nFound %s at %s serving %q. Add it with:\n  overload settings set --name %s --url %s --model %s%s --default\n", server.kind, server.url, server.model, server.name, server.url, server.model, server.flags)
	}
	fmt.Printf("\nNext: open %s, add a model under Models, and connect GitHub under GitHub.\nStop with `overload uninstall` (add --purge to delete data).\n", base)
	return nil
}

func (inst *installation) configure(port int, mode, databaseURL, postgresBin string) error {
	if port < 1 || port > 65535 {
		return errors.New("invalid --port")
	}
	password, err := randomSecret()
	if err != nil {
		return err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	inst.values = map[string]string{
		"OVERLOAD_ADDR":          addr,
		"OVERLOAD_BASE_URL":      "http://" + addr,
		"OVERLOAD_UI_USER":       "admin",
		"OVERLOAD_UI_PASSWORD":   password,
		"OVERLOAD_INSTALL_LABEL": inst.label,
	}
	if databaseURL != "" {
		mode = "url"
	}
	if mode == "auto" {
		if postgresBinDir(postgresBin) == "" {
			return errors.New("no Postgres found: install it with `brew install postgresql@17`, or pass --database-url")
		}
		mode = "native"
	}
	switch mode {
	case "url":
		if databaseURL == "" {
			return errors.New("--database url needs --database-url")
		}
		inst.values["OVERLOAD_POSTGRES"] = "external"
		inst.values["DATABASE_URL"] = databaseURL
		return nil
	case "native":
	default:
		return fmt.Errorf("unknown --database %q", mode)
	}
	dbPassword, err := randomSecret()
	if err != nil {
		return err
	}
	pgPort, err := freePort(firstPostgresPort)
	if err != nil {
		return err
	}
	inst.values["OVERLOAD_POSTGRES"] = mode
	inst.values["OVERLOAD_PG_PORT"] = strconv.Itoa(pgPort)
	inst.values["DATABASE_URL"] = fmt.Sprintf("postgres://overload:%s@127.0.0.1:%d/overload?sslmode=disable", dbPassword, pgPort)
	dir := postgresBinDir(postgresBin)
	if dir == "" {
		return errors.New("initdb and postgres not found; pass --postgres-bin or install postgresql@17")
	}
	inst.values["OVERLOAD_POSTGRES_BIN"] = dir
	inst.values["OVERLOAD_POSTGRES_DATA"] = filepath.Join(inst.home, "postgres")
	return nil
}

func (inst *installation) databasePassword() (string, error) {
	parsed, err := pgx.ParseConfig(inst.values["DATABASE_URL"])
	if err != nil {
		return "", err
	}
	return parsed.Password, nil
}

// preparePostgres creates the database cluster or container on first run.
func (inst *installation) preparePostgres(ctx context.Context) error {
	if inst.values["OVERLOAD_POSTGRES"] != "native" {
		return nil
	}
	data := inst.values["OVERLOAD_POSTGRES_DATA"]
	if _, err := os.Stat(filepath.Join(data, "PG_VERSION")); err == nil {
		return nil
	}
	password, err := inst.databasePassword()
	if err != nil {
		return err
	}
	pwfile := filepath.Join(inst.home, ".initdb-password")
	if err := os.WriteFile(pwfile, []byte(password+"\n"), 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(pwfile) }()
	cmd := exec.CommandContext(ctx, filepath.Join(inst.values["OVERLOAD_POSTGRES_BIN"], "initdb"), "-D", data, "-U", "overload", "--auth=scram-sha-256", "--pwfile="+pwfile, "-E", "UTF8", "--locale=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("initdb: %v\n%s", err, out)
	}
	return nil
}

// migrate starts a native Postgres temporarily if needed, creates the
// database and runs migrations.
func (inst *installation) migrate(ctx context.Context) error {
	if inst.values["OVERLOAD_POSTGRES"] == "native" {
		stop, err := inst.temporaryPostgres(ctx)
		if err != nil {
			return err
		}
		defer stop()
		if err := inst.createDatabase(ctx); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		store, err := postgres.Open(ctx, inst.values["DATABASE_URL"])
		if err == nil {
			defer store.Pool.Close()
			return store.Migrate(ctx)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("connect to database: %w", err)
		}
		time.Sleep(time.Second)
	}
}

func (inst *installation) postgresArgs() []string {
	data := inst.values["OVERLOAD_POSTGRES_DATA"]
	return []string{filepath.Join(inst.values["OVERLOAD_POSTGRES_BIN"], "postgres"), "-D", data, "-p", inst.values["OVERLOAD_PG_PORT"], "-c", "listen_addresses=127.0.0.1", "-c", "unix_socket_directories="}
}

func (inst *installation) temporaryPostgres(ctx context.Context) (func(), error) {
	addr := net.JoinHostPort("127.0.0.1", inst.values["OVERLOAD_PG_PORT"])
	if busy(addr) {
		return func() {}, nil
	}
	args := inst.postgresArgs()
	cmd := exec.Command(args[0], args[1:]...)
	logFile, err := os.OpenFile(filepath.Join(inst.home, "logs", "postgres-setup.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	stop := func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
		_ = logFile.Close()
	}
	for start := time.Now(); !busy(addr); {
		if time.Since(start) > 30*time.Second || ctx.Err() != nil {
			stop()
			return nil, errors.New("postgres did not start; see logs/postgres-setup.log")
		}
		time.Sleep(200 * time.Millisecond)
	}
	return stop, nil
}

func (inst *installation) createDatabase(ctx context.Context) error {
	config, err := pgx.ParseConfig(inst.values["DATABASE_URL"])
	if err != nil {
		return err
	}
	config.Database = "postgres"
	var conn *pgx.Conn
	for start := time.Now(); ; time.Sleep(200 * time.Millisecond) {
		if conn, err = pgx.ConnectConfig(ctx, config); err == nil || time.Since(start) > 15*time.Second {
			break
		}
	}
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname='overload')`).Scan(&exists); err != nil || exists {
		return err
	}
	_, err = conn.Exec(ctx, `CREATE DATABASE overload`)
	return err
}

func (inst *installation) agents() map[string]string {
	agents := map[string]string{inst.label: ""}
	if inst.values["OVERLOAD_POSTGRES"] == "native" {
		agents[inst.label+".postgres"] = ""
	}
	return agents
}

func launchAgentPath(label string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func (inst *installation) startServices(exe string) error {
	logs := filepath.Join(inst.home, "logs")
	if inst.values["OVERLOAD_POSTGRES"] == "native" {
		label := inst.label + ".postgres"
		if err := writePlist(launchAgentPath(label), launchAgent{Label: label, Args: inst.postgresArgs(), Dir: inst.home, Log: filepath.Join(logs, "postgres.log")}); err != nil {
			return err
		}
		if err := launchctl("bootstrap", guiDomain(), launchAgentPath(label)); err != nil {
			return err
		}
	}
	path := "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	agent := launchAgent{Label: inst.label, Args: []string{exe, "serve"}, Dir: inst.home, Log: filepath.Join(logs, "overload.log"), Env: map[string]string{"OVERLOAD_CONFIG": inst.config, "PATH": path}}
	if err := writePlist(launchAgentPath(inst.label), agent); err != nil {
		return err
	}
	return launchctl("bootstrap", guiDomain(), launchAgentPath(inst.label))
}

func (inst *installation) stopServices() {
	for _, label := range []string{inst.label, inst.label + ".postgres"} {
		_ = launchctl("bootout", guiDomain()+"/"+label)
	}
}

func uninstall(args []string) error {
	flags, home, label := installFlags("uninstall")
	purge := flags.Bool("purge", false, "also delete the database, configuration and logs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	inst, err := openInstallation(*home, *label)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		inst.stopServices()
		for _, label := range []string{inst.label, inst.label + ".postgres"} {
			if err := os.Remove(launchAgentPath(label)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if !*purge {
		fmt.Printf("Stopped overload. Data and configuration are kept in %s (use --purge to delete).\n", inst.home)
		return nil
	}
	if len(inst.values) == 0 && *home == "" {
		fmt.Println("No install found.")
		return nil
	}
	if err := os.RemoveAll(inst.home); err != nil {
		return err
	}
	fmt.Printf("Removed overload and its data from %s.\n", inst.home)
	return nil
}

func status(args []string) error {
	flags, home, label := installFlags("status")
	if err := flags.Parse(args); err != nil {
		return err
	}
	inst, err := openInstallation(*home, *label)
	if err != nil {
		return err
	}
	if len(inst.values) == 0 {
		fmt.Printf("Not installed (no %s). Run `overload install`.\n", inst.config)
		return nil
	}
	fmt.Printf("config:   %s\ndatabase: %s\n", inst.config, inst.values["OVERLOAD_POSTGRES"])
	if runtime.GOOS == "darwin" {
		for _, label := range []string{inst.label + ".postgres", inst.label} {
			if _, ok := inst.agents()[label]; ok {
				fmt.Printf("service:  %s %s\n", label, serviceState(label))
			}
		}
	}
	base := inst.values["OVERLOAD_BASE_URL"]
	if err := waitHealthy(context.Background(), base+"/healthz", 0); err != nil {
		fmt.Printf("ui:       %s (not responding: %v)\n", base, err)
		return nil
	}
	fmt.Printf("ui:       %s (healthy)\n", base)
	return nil
}

func serviceState(label string) string {
	out, err := exec.Command("launchctl", "print", guiDomain()+"/"+label).CombinedOutput()
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

func guiDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func serviceBinary(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if onPath, err := exec.LookPath("overload"); err == nil {
		resolvedPath, _ := filepath.EvalSymlinks(onPath)
		resolvedExe, _ := filepath.EvalSymlinks(exe)
		if resolvedPath != "" && resolvedPath == resolvedExe {
			return filepath.Abs(onPath)
		}
	}
	if strings.Contains(exe, "go-build") {
		return "", errors.New("run install from a built binary (not go run), or pass --binary")
	}
	return exe, nil
}

func postgresBinDir(override string) string {
	candidates := []string{override}
	for _, prefix := range []string{"/opt/homebrew/opt", "/usr/local/opt"} {
		for _, version := range []string{"18", "17", "16", "15", "14"} {
			candidates = append(candidates, filepath.Join(prefix, "postgresql@"+version, "bin"))
		}
	}
	candidates = append(candidates, "/Applications/Postgres.app/Contents/Versions/latest/bin")
	if path, err := exec.LookPath("initdb"); err == nil {
		candidates = append(candidates, filepath.Dir(path))
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if isExecutable(filepath.Join(dir, "initdb")) && isExecutable(filepath.Join(dir, "postgres")) {
			return dir
		}
	}
	return ""
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func randomSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func busy(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// waitFree waits for stopped services to release their ports.
func waitFree(addr, postgresPort string) error {
	addrs := []string{addr}
	if postgresPort != "" {
		addrs = append(addrs, net.JoinHostPort("127.0.0.1", postgresPort))
	}
	for _, addr := range addrs {
		for start := time.Now(); addr != "" && busy(addr); time.Sleep(200 * time.Millisecond) {
			if time.Since(start) > 20*time.Second {
				return fmt.Errorf("%s is still in use", addr)
			}
		}
	}
	return nil
}

func freePort(start int) (int, error) {
	for port := start; port < start+100; port++ {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			_ = listener.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free port near %d", start)
}

func waitHealthy(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			err = errors.New(response.Status)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Second)
	}
}

type localModelServer struct {
	kind, name, url, model, flags string
}

// detectLocalModels looks for model servers on their default ports: DwarfStar
// ds4-server on 8000 and llama.cpp's llama-server on 8080.
func detectLocalModels() []localModelServer {
	var found []localModelServer
	for _, url := range []string{"http://127.0.0.1:8000/v1", "http://127.0.0.1:8080/v1"} {
		model, owner := firstModel(url)
		if model == "" {
			continue
		}
		if owner == "ds4.c" {
			found = append(found, localModelServer{kind: "DwarfStar ds4-server", name: "ds4", url: url, model: model, flags: " --reasoning-param reasoning_effort --reasoning-effort high --max-output-tokens 28000"})
			continue
		}
		found = append(found, localModelServer{kind: "a local model server", name: "local", url: url, model: model})
	}
	return found
}

func firstModel(baseURL string) (string, string) {
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(baseURL + "/models")
	if err != nil {
		return "", ""
	}
	defer func() { _ = response.Body.Close() }()
	var models struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&models) != nil || len(models.Data) == 0 {
		return "", ""
	}
	return models.Data[0].ID, models.Data[0].OwnedBy
}
