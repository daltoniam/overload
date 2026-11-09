package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/launchd"
	"github.com/daltoniam/overload/localmodels"
	"github.com/daltoniam/overload/web/templates/pages"
)

const modelsUsage = `usage: overload models COMMAND
  pull ID --quant Q [--repository OWNER/NAME]   download and verify weights
  list                                          downloads, disk used and free
  rm ID [--quant Q]                             delete downloaded weights
  clean                                         delete partial downloads
  check ID                                      is llama-server new enough?
  serve ID [--quant Q] [--port 8080] [--parallel 1] [--connect NAME]
  stop ID                                       remove a model service
  status                                        services and their health
Every command takes --dir; check and serve take --llama-server`

// modelsCommand holds what the models subcommands depend on, so tests can
// replace the hub, service manager and output.
type modelsCommand struct {
	out     io.Writer
	hub     localmodels.Hub
	manager localmodels.ServiceManager
	client  *http.Client
	goos    string
	// connect saves a model connection for a started service.
	connect func(context.Context, overload.ReviewSettings) error
	// unreachable is how long serve waits for the server to answer at all.
	unreachable time.Duration
}

func manageModels(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	command := modelsCommand{out: os.Stdout, hub: localmodels.DefaultHub(huggingFaceToken()), manager: localmodels.Launchd{}, client: &http.Client{Timeout: 5 * time.Second}, goos: runtime.GOOS, connect: saveConnection, unreachable: time.Minute}
	return command.run(ctx, args)
}

// huggingFaceToken reads HF_TOKEN, or the token the hf CLI saved.
func huggingFaceToken() string {
	if token := strings.TrimSpace(os.Getenv("HF_TOKEN")); token != "" {
		return token
	}
	path := os.Getenv("HF_TOKEN_PATH")
	if path == "" {
		home := os.Getenv("HF_HOME")
		if home == "" {
			if user, err := os.UserHomeDir(); err == nil {
				home = filepath.Join(user, ".cache", "huggingface")
			}
		}
		path = filepath.Join(home, "token")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveConnection(ctx context.Context, setting overload.ReviewSettings) error {
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	return store.SaveReviewSettings(ctx, setting)
}

// modelsDir is where weights are kept: --dir, OVERLOAD_MODELS_DIR, or
// "models" next to the install's configuration, made absolute.
func modelsDir(flagValue string) (string, error) {
	dir := flagValue
	if dir == "" {
		dir = os.Getenv("OVERLOAD_MODELS_DIR")
	}
	if dir == "" {
		config := configPath()
		if config == "" {
			return "", errors.New("no install directory; pass --dir or set OVERLOAD_MODELS_DIR")
		}
		dir = filepath.Join(filepath.Dir(config), "models")
	}
	return filepath.Abs(dir)
}

func llamaServerBinary(flagValue string) (string, error) {
	binary := flagValue
	if binary == "" {
		binary = os.Getenv("OVERLOAD_LLAMA_SERVER")
	}
	if binary == "" {
		found, err := exec.LookPath("llama-server")
		if err != nil {
			return "", errors.New("llama-server not found; install llama.cpp, or pass --llama-server or set OVERLOAD_LLAMA_SERVER")
		}
		binary = found
	}
	return filepath.Abs(binary)
}

// localModel returns the catalog entry for a command that runs a model on
// this machine.
func localModel(id string) (catalog.Model, error) {
	model, ok := catalog.Lookup(id)
	if !ok {
		return catalog.Model{}, fmt.Errorf("catalog entry %q does not exist; see overload catalog list", id)
	}
	if model.Kind == catalog.KindHosted {
		return catalog.Model{}, fmt.Errorf("%s is a hosted model; add it with overload settings set instead", id)
	}
	return model, nil
}

// modelFlags parses one subcommand's flags, which may come before or after
// its ID.
type modelFlags struct {
	set        *flag.FlagSet
	dir        *string
	positional []string
}

func newModelFlags(name string) modelFlags {
	set := flag.NewFlagSet("models "+name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return modelFlags{set: set, dir: set.String("dir", "", "model directory (default: models next to the install's configuration, or OVERLOAD_MODELS_DIR)")}
}

func (flags *modelFlags) parse(args []string, ids int) error {
	for len(args) > 0 {
		if err := flags.set.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return errors.New(modelsUsage)
			}
			return err
		}
		args = flags.set.Args()
		if len(args) > 0 {
			flags.positional = append(flags.positional, args[0])
			args = args[1:]
		}
	}
	if len(flags.positional) != ids {
		if ids == 1 {
			return fmt.Errorf("usage: overload models %s ID", flags.set.Name()[len("models "):])
		}
		return fmt.Errorf("usage: overload models %s", flags.set.Name()[len("models "):])
	}
	return nil
}

func (flags modelFlags) store() (localmodels.Store, error) {
	dir, err := modelsDir(*flags.dir)
	if err != nil {
		return localmodels.Store{}, err
	}
	return localmodels.NewStore(dir), nil
}

func (command modelsCommand) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(modelsUsage)
	}
	switch args[0] {
	case "pull":
		return command.pull(ctx, args[1:])
	case "list":
		return command.list(args[1:])
	case "rm":
		flags := newModelFlags("rm")
		quant := flags.set.String("quant", "", "quantization to delete when several are downloaded")
		if err := flags.parse(args[1:], 1); err != nil {
			return err
		}
		store, err := flags.store()
		if err != nil {
			return err
		}
		removed, err := store.Remove(flags.positional[0], *quant)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.out, "Removed %s %s, freeing up to %s\n", removed.ID, removed.Quant, gigabytes(removed.Size()))
		return err
	case "clean":
		flags := newModelFlags("clean")
		if err := flags.parse(args[1:], 0); err != nil {
			return err
		}
		store, err := flags.store()
		if err != nil {
			return err
		}
		freed, err := store.Clean()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.out, "Removed partial downloads, freeing %s\n", gigabytes(freed))
		return err
	case "check":
		flags := newModelFlags("check")
		binary := flags.set.String("llama-server", "", "llama-server executable (default: OVERLOAD_LLAMA_SERVER or PATH)")
		if err := flags.parse(args[1:], 1); err != nil {
			return err
		}
		model, err := localModel(flags.positional[0])
		if err != nil {
			return err
		}
		version, err := command.runtime(ctx, model, *binary, false)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.out, "llama.cpp b%d (%s) can run %s\n", version.Build, version.Commit, model.ID)
		return err
	case "serve":
		return command.serve(ctx, args[1:])
	case "stop":
		flags := newModelFlags("stop")
		if err := flags.parse(args[1:], 1); err != nil {
			return err
		}
		store, err := flags.store()
		if err != nil {
			return err
		}
		if err := store.StopService(command.manager, flags.positional[0]); err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.out, "Stopped %s\n", flags.positional[0])
		return err
	case "status":
		return command.status(ctx, args[1:])
	default:
		return errors.New(modelsUsage)
	}
}

func (command modelsCommand) pull(ctx context.Context, args []string) error {
	flags := newModelFlags("pull")
	quant := flags.set.String("quant", "", "quantization, such as Q4_K_M or Q8_0")
	repository := flags.set.String("repository", "", "Hugging Face repository to download from instead of the catalog's")
	if err := flags.parse(args, 1); err != nil {
		return err
	}
	model, err := localModel(flags.positional[0])
	if err != nil {
		return err
	}
	if *quant == "" {
		return errors.New("choose a quantization with --quant; see overload catalog show " + model.ID)
	}
	store, err := flags.store()
	if err != nil {
		return err
	}
	reported := map[string]int64{}
	installed, err := store.Pull(ctx, command.hub, model, *quant, *repository, func(path string, done, total int64, verifying bool) {
		if verifying && done > 0 {
			_, _ = fmt.Fprintf(command.out, "%s: resuming at %s of %s\n", path, gigabytes(done), gigabytes(total))
			return
		}
		if verifying {
			_, _ = fmt.Fprintf(command.out, "%s: already downloaded, verifying %s\n", path, gigabytes(total))
			return
		}
		step := total / 20
		if step == 0 || done == total || done-reported[path] >= step {
			reported[path] = done
			_, _ = fmt.Fprintf(command.out, "%s: %s of %s\n", path, gigabytes(done), gigabytes(total))
		}
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(command.out, "Downloaded %s %s (%s, revision %.12s) to %s\n", installed.ID, installed.Quant, gigabytes(installed.Size()), installed.Revision, store.Dir)
	if err == nil && installed.Warning != "" {
		_, err = fmt.Fprintf(command.out, "Warning: %s\n", installed.Warning)
	}
	switch {
	case err != nil:
	case installed.Served:
		_, err = fmt.Fprintf(command.out, "%s is being served from the previous download. Run overload models serve %s to load the update; its old files are kept until then; overload models clean deletes them afterwards.\n", installed.ID, installed.ID)
	case installed.Freed > 0:
		_, err = fmt.Fprintf(command.out, "Deleted %s of files the previous download used.\n", gigabytes(installed.Freed))
	}
	return err
}

func (command modelsCommand) list(args []string) error {
	flags := newModelFlags("list")
	if err := flags.parse(args, 0); err != nil {
		return err
	}
	store, err := flags.store()
	if err != nil {
		return err
	}
	installed, err := store.List()
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(command.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tQUANT\tSIZE\tREPOSITORY\tDOWNLOADED")
	for _, entry := range installed {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", entry.ID, entry.Quant, gigabytes(entry.Size()), entry.Repository, entry.DownloadedAt.Format("2006-01-02"))
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	used, free, err := store.Usage()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(command.out, "%s used in %s; %s free on the volume\n", gigabytes(used), store.Dir, gigabytes(free))
	return err
}

func (command modelsCommand) status(ctx context.Context, args []string) error {
	flags := newModelFlags("status")
	if err := flags.parse(args, 0); err != nil {
		return err
	}
	store, err := flags.store()
	if err != nil {
		return err
	}
	services, err := store.Services()
	if err != nil {
		return err
	}
	if len(services) == 0 {
		_, err := fmt.Fprintln(command.out, "No model services. Start one with overload models serve ID.")
		return err
	}
	writer := tabwriter.NewWriter(command.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tQUANT\tENDPOINT\tMODEL\tSERVICE\tHEALTH")
	for _, service := range services {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", service.ID, service.Quant, service.BaseURL(), service.ModelName, command.manager.State(service.Label), localmodels.CheckHealth(ctx, command.client, service))
	}
	return writer.Flush()
}

// runtime checks the llama-server build against the catalog entry.
func (command modelsCommand) runtime(ctx context.Context, model catalog.Model, binaryFlag string, skip bool) (localmodels.LlamaVersion, error) {
	binary, err := llamaServerBinary(binaryFlag)
	if err != nil {
		return localmodels.LlamaVersion{}, err
	}
	version, err := localmodels.DetectLlama(ctx, binary)
	if err != nil {
		return localmodels.LlamaVersion{}, err
	}
	if skip && version.Build == 0 {
		return version, nil
	}
	return version, localmodels.CheckRuntime(model, version)
}

func (command modelsCommand) serve(ctx context.Context, args []string) error {
	flags := newModelFlags("serve")
	quant := flags.set.String("quant", "", "quantization to serve when several are downloaded")
	binaryFlag := flags.set.String("llama-server", "", "llama-server executable (default: OVERLOAD_LLAMA_SERVER or PATH)")
	port := flags.set.Int("port", 8080, "port the model server listens on, on 127.0.0.1")
	parallel := flags.set.Int("parallel", 1, "parallel request slots; each gets the catalog's full context")
	connect := flags.set.String("connect", "", "also save a model connection with this name (needs DATABASE_URL)")
	wait := flags.set.Duration("wait", 15*time.Minute, "how long to wait for the model to load")
	skip := flags.set.Bool("skip-version-check", false, "start a llama-server that reports no build number")
	if err := flags.parse(args, 1); err != nil {
		return err
	}
	if command.goos != "darwin" {
		return errors.New("model services use launchd and need macOS; on Linux run llama-server under systemd or in Kubernetes")
	}
	model, err := localModel(flags.positional[0])
	if err != nil {
		return err
	}
	if model.Runtime == nil || model.Runtime.Engine != "llama.cpp" {
		return fmt.Errorf("%s does not run on llama.cpp", model.ID)
	}
	store, err := flags.store()
	if err != nil {
		return err
	}
	installed, err := store.Find(model.ID, *quant)
	if err != nil {
		return err
	}
	if _, err := command.runtime(ctx, model, *binaryFlag, *skip); err != nil {
		return err
	}
	binary, err := llamaServerBinary(*binaryFlag)
	if err != nil {
		return err
	}
	label := os.Getenv("OVERLOAD_INSTALL_LABEL")
	if label == "" {
		label = defaultLabel
	}
	logDir, err := filepath.Abs(filepath.Join(filepath.Dir(configPath()), "logs"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	agent, service, err := store.ServiceAgent(model, installed, localmodels.ServeOptions{Binary: binary, LabelPrefix: label, Port: *port, Parallel: *parallel, LogDir: logDir})
	if err != nil {
		return err
	}
	if err := store.Serve(command.manager, agent, service); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(command.out, "Started %s %s at %s; waiting for it to load (logs in %s)\n", model.ID, installed.Quant, service.BaseURL(), agent.Log)
	waitCtx, cancel := context.WithTimeout(ctx, *wait)
	defer cancel()
	if err := localmodels.WaitReady(waitCtx, command.client, service, 2*time.Second, command.unreachable); err != nil {
		return fmt.Errorf("%w; the service is still registered (overload models stop %s removes it). Last log lines:\n%s", err, model.ID, logTail(agent.Log, 20))
	}
	if err := localmodels.CheckModel(ctx, command.client, service); err != nil {
		return fmt.Errorf("%w; the service is still registered (overload models stop %s removes it). Last log lines:\n%s", err, model.ID, logTail(agent.Log, 20))
	}
	if *connect == "" {
		_, err := fmt.Fprintf(command.out, "Ready. Add it with: overload settings set --name %s --url %s --model %s --catalog %s --concurrency %d\n", model.ID, service.BaseURL(), service.ModelName, model.ID, *parallel)
		return err
	}
	setting := overload.ReviewSettings{Name: *connect, Provider: "openaicompat", ConnectionKind: "local", BaseURL: service.BaseURL(), Model: service.ModelName, PromptProfile: "context", Concurrency: *parallel, Catalog: model.ID}
	if err := applyCatalogDefaults(&setting, map[string]bool{"model": true}); err != nil {
		return err
	}
	if err := command.connect(ctx, setting); err != nil {
		return fmt.Errorf("the model is running, but saving the %s connection failed: %w", *connect, err)
	}
	_, err = fmt.Fprintf(command.out, "Ready, and saved as model connection %s\n", *connect)
	return err
}

// logTail returns the last lines of a log file.
func logTail(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no log yet)"
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

func gigabytes(size int64) string {
	return fmt.Sprintf("%.1f GB", float64(size)/1e9)
}

// localModelSummary describes what store holds of a catalog entry for the
// catalog page. A store that cannot be read is reported on the page rather
// than failing it.
func localModelSummary(store localmodels.Store, manager localmodels.ServiceManager, id string) *pages.LocalModel {
	summary := &pages.LocalModel{}
	installed, err := store.List()
	if err != nil {
		summary.Error = "The model directory could not be read: " + err.Error()
		return summary
	}
	var size int64
	for _, entry := range installed {
		if entry.ID == id {
			summary.Quants = append(summary.Quants, entry.Quant)
			size += entry.Size()
		}
	}
	summary.Size = gigabytes(size)
	services, err := store.Services()
	if err != nil {
		summary.Error = "The model services could not be read: " + err.Error()
		return summary
	}
	for _, service := range services {
		if service.ID == id {
			summary.Endpoint, summary.Service = service.BaseURL(), manager.State(service.Label)
		}
	}
	return summary
}

// removeModelServices removes the launchd services models started for an
// install, for uninstall: those recorded in the model directory under the
// install's label, and any other agent under that label in case the
// directory moved. Services of other installs are left alone.
func removeModelServices(dir, label string) error {
	prefix := label + ".model."
	removed, err := localmodels.NewStore(dir).RemoveServices(localmodels.Launchd{}, label)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Printf("Could not update %s (%v); removing model services by label instead.\n", dir, err)
	}
	var failures []error
	plists, _ := filepath.Glob(filepath.Join(filepath.Dir(launchd.Path(label)), prefix+"*.plist"))
	for _, plist := range plists {
		service := strings.TrimSuffix(filepath.Base(plist), ".plist")
		if slices.Contains(removed, service) {
			continue
		}
		if err := launchd.Remove(service); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
