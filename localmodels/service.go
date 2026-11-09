package localmodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/daltoniam/overload/catalog"
	"github.com/daltoniam/overload/launchd"
)

// Service is a llama-server login service for one installed model.
type Service struct {
	ID       string `json:"id"`
	Quant    string `json:"quant"`
	Label    string `json:"label"`
	Port     int    `json:"port"`
	Parallel int    `json:"parallel"`
	// ModelName is the name requests use (llama-server --alias).
	ModelName string `json:"model_name"`
	// Files are the weights the service loads, so clean-up keeps them
	// while it runs even after a newer download replaced them.
	Files []string `json:"files,omitempty"`
}

// BaseURL is the service's OpenAI-compatible endpoint.
func (service Service) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", service.Port)
}

// ServiceManager installs and controls login services; launchd on macOS.
type ServiceManager interface {
	Write(launchd.Agent) error
	Start(label string) error
	Stop(label string)
	Remove(label string) error
	State(label string) string
}

// Launchd manages services with launchd.
type Launchd struct{}

func (Launchd) Write(agent launchd.Agent) error { return launchd.Write(agent) }
func (Launchd) Start(label string) error        { return launchd.Start(label) }
func (Launchd) Stop(label string)               { launchd.Stop(label) }
func (Launchd) Remove(label string) error       { return launchd.Remove(label) }
func (Launchd) State(label string) string       { return launchd.State(label) }

// ServeOptions configure a model service.
type ServeOptions struct {
	// Binary is the llama-server executable.
	Binary string
	// LabelPrefix is the install's launchd label; services use
	// <prefix>.model.<id>.
	LabelPrefix string
	Port        int
	// Parallel is llama-server's parallel slots; each gets the catalog's
	// full context.
	Parallel int
	LogDir   string
}

// ServiceAgent builds the launchd agent that runs llama-server for an
// installed model with the catalog's known-good arguments.
func (store Store) ServiceAgent(model catalog.Model, installed Installed, options ServeOptions) (launchd.Agent, Service, error) {
	if model.Runtime == nil || model.Runtime.Engine != "llama.cpp" {
		return launchd.Agent{}, Service{}, fmt.Errorf("%s does not run on llama.cpp", model.ID)
	}
	if options.Port < 1024 || options.Port > 65535 {
		return launchd.Agent{}, Service{}, errors.New("port must be between 1024 and 65535")
	}
	if options.Parallel < 1 || options.Parallel > 32 {
		return launchd.Agent{}, Service{}, errors.New("parallel slots must be between 1 and 32")
	}
	if len(installed.Files) == 0 {
		return launchd.Agent{}, Service{}, errors.New("the installed model has no files")
	}
	for _, path := range []string{store.Dir, options.Binary, options.LogDir} {
		if !filepath.IsAbs(path) {
			return launchd.Agent{}, Service{}, fmt.Errorf("%q must be an absolute path: launchd does not start services in this directory", path)
		}
	}
	name := model.ID
	if model.Runtime.ServedName != "" {
		name = model.Runtime.ServedName
	}
	service := Service{ID: model.ID, Quant: installed.Quant, Label: options.LabelPrefix + ".model." + model.ID, Port: options.Port, Parallel: options.Parallel, ModelName: name}
	for _, file := range installed.Files {
		service.Files = append(service.Files, store.FilePath(installed, file))
	}
	args := []string{options.Binary, "-m", service.Files[0], "--host", "127.0.0.1", "--port", strconv.Itoa(options.Port), "--alias", name}
	args = append(args, scaleContext(model.Runtime.ServerArgs, options.Parallel)...)
	// Always set the slot count: newer llama-server builds pick several
	// slots on their own, which would divide the context.
	args = append(args, "-np", strconv.Itoa(options.Parallel))
	agent := launchd.Agent{Label: service.Label, Args: args, Dir: store.Dir, Log: filepath.Join(options.LogDir, "model-"+model.ID+".log")}
	return agent, service, nil
}

// scaleContext multiplies the -c context so that each of parallel slots
// gets the catalog's full window (llama-server divides -c among slots).
func scaleContext(args []string, parallel int) []string {
	out := slices.Clone(args)
	for index := 0; index+1 < len(out); index++ {
		if out[index] == "-c" || out[index] == "--ctx-size" {
			if size, err := strconv.Atoi(out[index+1]); err == nil {
				out[index+1] = strconv.Itoa(size * parallel)
			}
		}
	}
	return out
}

// PortInUse reports whether something answers on a local port; a variable
// so tests can replace it.
var PortInUse = func(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// portFreeSoon waits a few seconds for a stopped server to release port.
func portFreeSoon(port int) error {
	for range 25 {
		if !PortInUse(port) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port %d is still in use", port)
}

// Serve installs and starts the service, replacing an earlier one for the
// same model, and records it. It refuses a port another service or
// program uses, and removes the service again if it cannot start.
func (store Store) Serve(ctx context.Context, manager ServiceManager, agent launchd.Agent, service Service) error {
	// A pull holds this lock briefly while it records a download; wait
	// for it rather than fail.
	unlock, err := store.lockWait(ctx, ".services.lock", time.Minute)
	if err != nil {
		return err
	}
	defer unlock()
	services, err := store.Services()
	if err != nil {
		return err
	}
	replacing := false
	for _, existing := range services {
		if existing.Label == service.Label && existing.ID != service.ID {
			return fmt.Errorf("the service name %s is already used by %s", service.Label, existing.ID)
		}
		if existing.Port == service.Port && existing.ID != service.ID {
			return fmt.Errorf("port %d is already used by the %s service", service.Port, existing.ID)
		}
		if existing.ID == service.ID && existing.Label != service.Label {
			return fmt.Errorf("%s is already served by another install (%s); stop it there first", service.ID, existing.Label)
		}
		if existing.ID == service.ID && existing.Port == service.Port {
			replacing = true
		}
	}
	if !replacing && PortInUse(service.Port) {
		return fmt.Errorf("port %d is already in use on this Mac; choose another with --port", service.Port)
	}
	if err := store.filesPresent(service); err != nil {
		return err
	}
	if replacing {
		// The recorded agent may have stopped and its port been taken
		// since: stop it and check the port is free before replacing its
		// configuration; restart it if not.
		manager.Stop(agent.Label)
		if portFreeSoon(service.Port) != nil {
			_ = manager.Start(agent.Label)
			return fmt.Errorf("port %d is in use by another program; choose another with --port", service.Port)
		}
	}
	previous := slices.Clone(services)
	hadService := slices.ContainsFunc(services, func(existing Service) bool { return existing.ID == service.ID })
	services = slices.DeleteFunc(services, func(existing Service) bool { return existing.ID == service.ID })
	// Record the service before starting it, so a running agent is never
	// missing from the registry; undo the record if it cannot start.
	if err := store.writeServices(append(services, service)); err != nil {
		return err
	}
	restore := func(cause error) error {
		if err := store.writeServices(previous); err != nil {
			return fmt.Errorf("%w; restoring the service list also failed: %v", cause, err)
		}
		return cause
	}
	manager.Stop(agent.Label)
	if err := manager.Write(agent); err != nil {
		// The agent file is written atomically, so a previous service's
		// file is intact; start it again.
		if hadService {
			_ = manager.Start(agent.Label)
		}
		return restore(fmt.Errorf("write the %s service: %w", service.ID, err))
	}
	if err := manager.Start(agent.Label); err != nil {
		if removeErr := manager.Remove(agent.Label); removeErr != nil {
			// The agent is still installed: keep its record so stop can
			// remove it later.
			return fmt.Errorf("start %s: %w; removing its service also failed (run overload models stop %s): %v", service.ID, err, service.ID, removeErr)
		}
		if writeErr := store.writeServices(services); writeErr != nil {
			return fmt.Errorf("start %s: %w; its service was removed but the service list could not be updated: %v", service.ID, err, writeErr)
		}
		return fmt.Errorf("start %s: %w; its service was removed", service.ID, err)
	}
	return nil
}

// filesPresent checks, under the services lock that rm, pull and clean
// also take, that the weights the agent was built to load are still the
// installed ones.
func (store Store) filesPresent(service Service) error {
	installed, err := store.Find(service.ID, service.Quant)
	if err != nil {
		return err
	}
	current := make([]string, 0, len(installed.Files))
	for _, file := range installed.Files {
		current = append(current, store.FilePath(installed, file))
	}
	if len(service.Files) == 0 || !slices.Equal(current, service.Files) {
		return fmt.Errorf("%s changed while the service was being prepared; run the command again", service.ID)
	}
	for _, path := range service.Files {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s is missing; run overload models pull %s again", path, service.ID)
		}
	}
	return nil
}

// StopService removes the model's service.
func (store Store) StopService(ctx context.Context, manager ServiceManager, id string) error {
	unlock, err := store.lockWait(ctx, ".services.lock", time.Minute)
	if err != nil {
		return err
	}
	defer unlock()
	services, err := store.Services()
	if err != nil {
		return err
	}
	index := slices.IndexFunc(services, func(service Service) bool { return service.ID == id })
	if index < 0 {
		return fmt.Errorf("%s has no service", id)
	}
	if err := manager.Remove(services[index].Label); err != nil {
		return err
	}
	return store.writeServices(slices.Delete(services, index, index+1))
}

// RemoveServices removes one install's services (labels under
// <label>.model.), keeping other installs' services that share the
// directory, and returns the labels it removed.
func (store Store) RemoveServices(manager ServiceManager, label string) ([]string, error) {
	prefix := label + ".model."
	if _, err := os.Stat(store.Dir); err != nil {
		return nil, err
	}
	unlock, err := store.lock(".services.lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	services, err := store.Services()
	if err != nil {
		return nil, err
	}
	var removed []string
	var failures []error
	kept := slices.DeleteFunc(services, func(service Service) bool {
		if !strings.HasPrefix(service.Label, prefix) {
			return false
		}
		if err := manager.Remove(service.Label); err != nil {
			failures = append(failures, err)
			return false
		}
		removed = append(removed, service.Label)
		return true
	})
	if err := store.writeServices(kept); err != nil {
		failures = append(failures, err)
	}
	return removed, errors.Join(failures...)
}

// Services lists recorded model services.
func (store Store) Services() ([]Service, error) {
	data, err := os.ReadFile(filepath.Join(store.Dir, "services.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var services []Service
	if err := json.Unmarshal(data, &services); err != nil {
		return nil, err
	}
	return services, nil
}

func (store Store) writeServices(services []Service) error {
	data, err := json.MarshalIndent(services, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(store.Dir, "services.json"), data)
}

// Health is a service's state as llama-server reports it.
type Health string

const (
	HealthReady       Health = "ready"
	HealthLoading     Health = "loading"
	HealthUnreachable Health = "unreachable"
)

// CheckHealth asks llama-server's /health endpoint: 200 when ready, 503
// while the model loads.
func CheckHealth(ctx context.Context, client *http.Client, service Service) Health {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(service.BaseURL(), "/v1")+"/health", nil)
	if err != nil {
		return HealthUnreachable
	}
	response, err := client.Do(request)
	if err != nil {
		return HealthUnreachable
	}
	_ = response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return HealthReady
	case http.StatusServiceUnavailable:
		return HealthLoading
	default:
		return HealthUnreachable
	}
}

// ErrNotStarting means the model server never answered.
var ErrNotStarting = errors.New("the model server is not starting")

// WaitReady polls the service until it is ready or ctx ends. A server that
// has not answered for longer than unreachable (since it started, or since
// it last answered while loading, as when it crashes mid-load and launchd
// restarts it) is reported as not starting, rather than waiting out ctx.
func WaitReady(ctx context.Context, client *http.Client, service Service, interval, unreachable time.Duration) error {
	lastAnswer := time.Now()
	for {
		switch CheckHealth(ctx, client, service) {
		case HealthReady:
			return nil
		case HealthLoading:
			lastAnswer = time.Now()
		case HealthUnreachable:
			if unreachable > 0 && time.Since(lastAnswer) > unreachable {
				return ErrNotStarting
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not become ready: %w", service.ID, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// CheckModel confirms the server on the service's port is the one serving
// this model, by its /v1/models list.
func CheckModel(ctx context.Context, client *http.Client, service Service) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, service.BaseURL()+"/models", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&list); err != nil {
		return fmt.Errorf("port %d does not answer like llama-server", service.Port)
	}
	for _, model := range list.Data {
		if model.ID == service.ModelName {
			return nil
		}
	}
	return fmt.Errorf("port %d serves a different model; another server may be using it", service.Port)
}
