package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/localmodels"
	"github.com/daltoniam/overload/web/templates/pages"
)

const modelsUsage = "usage: overload models pull ID --quant Q | list | rm ID | clean | check ID | serve ID | stop ID | status"

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
}

func manageModels(args []string) error {
	command := modelsCommand{out: os.Stdout, hub: localmodels.DefaultHub(os.Getenv("HF_TOKEN")), manager: localmodels.Launchd{}, client: &http.Client{Timeout: 5 * time.Second}, goos: runtime.GOOS, connect: saveConnection}
	return command.run(context.Background(), args)
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
// "models" next to the install's configuration.
func modelsDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if dir := os.Getenv("OVERLOAD_MODELS_DIR"); dir != "" {
		return dir
	}
	if config := configPath(); config != "" {
		return filepath.Join(filepath.Dir(config), "models")
	}
	return "models"
}

func llamaServerBinary(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if binary := os.Getenv("OVERLOAD_LLAMA_SERVER"); binary != "" {
		return binary, nil
	}
	binary, err := exec.LookPath("llama-server")
	if err != nil {
		return "", errors.New("llama-server not found; install llama.cpp, or pass --llama-server or set OVERLOAD_LLAMA_SERVER")
	}
	return binary, nil
}

func lookupModel(id string) (catalog.Model, error) {
	model, ok := catalog.Lookup(id)
	if !ok {
		return catalog.Model{}, fmt.Errorf("catalog entry %q does not exist; see overload catalog list", id)
	}
	return model, nil
}

func (command modelsCommand) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(modelsUsage)
	}
	flags := flag.NewFlagSet("models "+args[0], flag.ContinueOnError)
	dir := flags.String("dir", "", "model directory (default: models next to the install's configuration, or OVERLOAD_MODELS_DIR)")
	quant := flags.String("quant", "", "quantization, such as Q4_K_M or Q8_0")
	repository := flags.String("repository", "", "Hugging Face repository to download from instead of the catalog's")
	binary := flags.String("llama-server", "", "llama-server executable (default: OVERLOAD_LLAMA_SERVER or PATH)")
	port := flags.Int("port", 8080, "port the model server listens on, on 127.0.0.1")
	parallel := flags.Int("parallel", 1, "parallel request slots; each gets the catalog's full context")
	connect := flags.String("connect", "", "also save a model connection with this name (needs DATABASE_URL)")
	wait := flags.Duration("wait", 15*time.Minute, "how long serve waits for the model to load")
	var positional []string
	rest := args[1:]
	for len(rest) > 0 {
		if err := flags.Parse(rest); err != nil {
			return err
		}
		rest = flags.Args()
		if len(rest) > 0 {
			positional = append(positional, rest[0])
			rest = rest[1:]
		}
	}
	store := localmodels.NewStore(modelsDir(*dir))
	needID := func() (catalog.Model, error) {
		if len(positional) != 1 {
			return catalog.Model{}, fmt.Errorf("usage: overload models %s ID", args[0])
		}
		return lookupModel(positional[0])
	}
	noArgs := func() error {
		if len(positional) != 0 {
			return fmt.Errorf("usage: overload models %s", args[0])
		}
		return nil
	}
	switch args[0] {
	case "pull":
		model, err := needID()
		if err != nil {
			return err
		}
		if *quant == "" {
			return errors.New("choose a quantization with --quant; see overload catalog show " + model.ID)
		}
		reported := map[string]int64{}
		installed, err := store.Pull(ctx, command.hub, model, *quant, *repository, func(path string, done, total int64) {
			step := total / 20
			if step == 0 || done == total || done-reported[path] >= step {
				reported[path] = done
				_, _ = fmt.Fprintf(command.out, "%s: %s of %s\n", path, gigabytes(done), gigabytes(total))
			}
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.out, "Downloaded %s %s (%s) to %s\n", installed.ID, installed.Quant, gigabytes(installed.Size()), store.Dir)
		return err
	case "list":
		if err := noArgs(); err != nil {
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
	case "rm":
		model, err := needID()
		if err != nil {
			return err
		}
		services, err := store.Services()
		if err != nil {
			return err
		}
		for _, service := range services {
			if service.ID == model.ID && (*quant == "" || strings.EqualFold(service.Quant, *quant)) {
				return fmt.Errorf("%s is being served; run overload models stop %s first", model.ID, model.ID)
			}
		}
		removed, err := store.Remove(model.ID, *quant)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.out, "Removed %s %s, freeing %s\n", removed.ID, removed.Quant, gigabytes(removed.Size()))
		return err
	case "clean":
		if err := noArgs(); err != nil {
			return err
		}
		freed, err := store.Clean()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.out, "Removed partial downloads, freeing %s\n", gigabytes(freed))
		return err
	case "check":
		model, err := needID()
		if err != nil {
			return err
		}
		path, err := llamaServerBinary(*binary)
		if err != nil {
			return err
		}
		version, err := localmodels.DetectLlama(ctx, path)
		if err != nil {
			return err
		}
		if err := localmodels.CheckRuntime(model, version); err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.out, "llama.cpp b%d (%s) can run %s\n", version.Build, version.Commit, model.ID)
		return err
	case "serve":
		return command.serve(ctx, store, positional, *quant, *binary, *port, *parallel, *connect, *wait)
	case "stop":
		model, err := needID()
		if err != nil {
			return err
		}
		if err := store.StopService(command.manager, model.ID); err != nil {
			return err
		}
		_, err = fmt.Fprintf(command.out, "Stopped %s\n", model.ID)
		return err
	case "status":
		if err := noArgs(); err != nil {
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
	default:
		return errors.New(modelsUsage)
	}
}

func (command modelsCommand) serve(ctx context.Context, store localmodels.Store, positional []string, quant, binaryFlag string, port, parallel int, connect string, wait time.Duration) error {
	if len(positional) != 1 {
		return errors.New("usage: overload models serve ID [--quant Q] [--port 8080] [--parallel N] [--connect NAME]")
	}
	if command.goos != "darwin" {
		return errors.New("model services use launchd and need macOS; on Linux run llama-server under systemd or in Kubernetes")
	}
	model, err := lookupModel(positional[0])
	if err != nil {
		return err
	}
	installed, err := store.Find(model.ID, quant)
	if err != nil {
		return err
	}
	binary, err := llamaServerBinary(binaryFlag)
	if err != nil {
		return err
	}
	if binary, err = filepath.Abs(binary); err != nil {
		return err
	}
	version, err := localmodels.DetectLlama(ctx, binary)
	if err != nil {
		return err
	}
	if err := localmodels.CheckRuntime(model, version); err != nil {
		return err
	}
	label := os.Getenv("OVERLOAD_INSTALL_LABEL")
	if label == "" {
		label = defaultLabel
	}
	logDir := filepath.Join(filepath.Dir(store.Dir), "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return err
	}
	agent, service, err := store.ServiceAgent(model, installed, localmodels.ServeOptions{Binary: binary, LabelPrefix: label, Port: port, Parallel: parallel, LogDir: logDir})
	if err != nil {
		return err
	}
	if err := store.Serve(command.manager, agent, service); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(command.out, "Started %s %s at %s; waiting for it to load (logs in %s)\n", model.ID, installed.Quant, service.BaseURL(), agent.Log)
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if err := localmodels.WaitReady(waitCtx, command.client, service, 2*time.Second); err != nil {
		return err
	}
	setting := overload.ReviewSettings{Name: connect, Provider: "openaicompat", ConnectionKind: "local", BaseURL: service.BaseURL(), Model: service.ModelName, PromptProfile: "context", Concurrency: parallel, Catalog: model.ID}
	if err := applyCatalogDefaults(&setting, map[string]bool{"model": true}); err != nil {
		return err
	}
	if connect == "" {
		_, err := fmt.Fprintf(command.out, "Ready. Add it with: overload settings set --name %s --url %s --catalog %s --concurrency %d\n", model.ID, service.BaseURL(), model.ID, parallel)
		return err
	}
	if err := command.connect(ctx, setting); err != nil {
		return fmt.Errorf("the model is running, but saving the %s connection failed: %w", connect, err)
	}
	_, err = fmt.Fprintf(command.out, "Ready, and saved as model connection %s\n", connect)
	return err
}

func gigabytes(size int64) string {
	return fmt.Sprintf("%.1f GB", float64(size)/1e9)
}

// localModelSummary describes what store holds of a catalog entry for the
// catalog page.
func localModelSummary(store localmodels.Store, manager localmodels.ServiceManager, id string) (*pages.LocalModel, error) {
	installed, err := store.List()
	if err != nil {
		return nil, err
	}
	summary := &pages.LocalModel{}
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
		return nil, err
	}
	for _, service := range services {
		if service.ID == id {
			summary.Endpoint, summary.Service = service.BaseURL(), manager.State(service.Label)
		}
	}
	return summary, nil
}
