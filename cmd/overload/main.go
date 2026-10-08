package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/daltoniam/overload/github"
	httpapi "github.com/daltoniam/overload/http"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/queue"
	"github.com/daltoniam/overload/tunnel"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

var version = "dev"

func main() {
	if err := loadConfig(configPath()); err != nil {
		slog.Error("overload failed", "error", err)
		os.Exit(1)
	}
	if err := run(os.Args[1:]); err != nil {
		slog.Error("overload failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: overload serve")
	}
	switch args[0] {
	case "serve":
		if len(args) != 1 {
			return errors.New("usage: overload serve")
		}
		return serve()
	case "migrate":
		return migrate()
	case "dev-seed":
		if len(args) != 1 {
			return errors.New("usage: overload dev-seed")
		}
		return devSeed()
	case "install-check":
		if len(args) != 1 {
			return errors.New("usage: overload install-check")
		}
		return installCheck()
	case "version":
		fmt.Println(version)
		return nil
	case "healthcheck":
		if len(args) != 1 {
			return errors.New("usage: overload healthcheck")
		}
		return healthcheck()
	case "install":
		return install(args[1:])
	case "uninstall":
		return uninstall(args[1:])
	case "status":
		return status(args[1:])
	case "review":
		return inlineReview(args[1:])
	case "settings":
		return manageSettings(args[1:])
	case "agents", "workflows", "bindings", "repositories", "schedules", "tools":
		return manageConfiguration(args[0], args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func database(ctx context.Context) (*postgres.Store, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("DATABASE_URL required")
	}
	return postgres.Open(ctx, dsn)
}

// waitForDatabase retries until Postgres accepts connections, so a service
// started before its database does not crash-loop.
func waitForDatabase(ctx context.Context, timeout time.Duration) (*postgres.Store, error) {
	deadline := time.Now().Add(timeout)
	for {
		store, err := database(ctx)
		if err == nil || os.Getenv("DATABASE_URL") == "" || time.Now().After(deadline) {
			return store, err
		}
		slog.Warn("waiting for database", "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	return store.Migrate(ctx)
}

func serve() error {
	if err := httpapi.ValidateAuth(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	store, err := waitForDatabase(ctx, 2*time.Minute)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	runner, err := sandboxFromEnvironment(ctx)
	if err != nil {
		return err
	}
	client, err := queue.NewClient(store, 2, int((260*time.Minute)/time.Second), runner)
	if err != nil {
		return err
	}
	if err := client.Start(ctx); err != nil {
		return err
	}
	go scanSchedules(ctx, store, client)
	defer func() {
		stop()
		<-client.Stopped()
	}()
	addr := os.Getenv("OVERLOAD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8082"
	}
	if err := validateServeAddress(addr, os.Getenv("OVERLOAD_UI_INSECURE") == "1"); err != nil {
		return err
	}
	server := &http.Server{Addr: addr, Handler: httpapi.Handler(serverStore{Store: store, tunnel: newTunnel(store, addr), jobs: client}, func(ctx context.Context, deliveryID, action string, payload []byte, event github.PullRequestEvent) (bool, error) {
		return store.IngestPR(ctx, client, postgres.PullRequestDelivery{
			DeliveryID:     deliveryID,
			Action:         action,
			RepoName:       event.Repository.FullName,
			RepoID:         event.Repository.ID,
			InstallationID: event.Installation.ID,
			PR:             event.Number,
			HeadSHA:        event.PullRequest.Head.SHA,
			BaseSHA:        event.PullRequest.Base.SHA,
			Eligible:       github.ShouldReview(event),
			Payload:        payload,
		})
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: time.Minute, WriteTimeout: time.Minute, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown failed", "error", err)
		}
	}()
	slog.Info("listening", "addr", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// serverStore adds GitHub lookups and the Cloudflare Tunnel the web UI needs
// to the database store.
type serverStore struct {
	*postgres.Store
	tunnel *tunnel.Manager
	jobs   *river.Client[pgx.Tx]
}

// RunScheduleNow queues a schedule's workflow on this server's job queue.
func (store serverStore) RunScheduleNow(ctx context.Context, name string) (int64, error) {
	return store.Store.RunScheduleNow(ctx, store.jobs, name)
}

func (store serverStore) Tunnel() httpapi.TunnelControl {
	if store.tunnel == nil {
		return nil
	}
	return store.tunnel
}

// newTunnel manages the tunnel for this install: files next to the config
// file, a launchd label derived from the install's, and the GitHub App's
// webhook URL updated once the tunnel works.
func newTunnel(store *postgres.Store, addr string) *tunnel.Manager {
	config := configPath()
	if config == "" {
		return nil
	}
	label := os.Getenv("OVERLOAD_INSTALL_LABEL")
	if label == "" {
		label = defaultLabel
	}
	manager := tunnel.NewManager(filepath.Dir(config), label, "http://"+addr)
	manager.OnReady = func(ctx context.Context, webhookURL string) (string, error) {
		if os.Getenv("GITHUB_APP_ID") != "" {
			return "Set the GitHub App's webhook URL to " + webhookURL + " (the App is configured in the environment).", nil
		}
		app, err := store.LoadGitHubApp(ctx)
		if errors.Is(err, postgres.ErrNoGitHubApp) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		client, err := github.NewClient(app.AppID, []byte(app.PrivateKey))
		if err != nil {
			return "", err
		}
		if err := client.SetWebhookURL(ctx, webhookURL); err != nil {
			return "", err
		}
		return "Pointed the GitHub App " + app.Slug + " at " + webhookURL + ".", nil
	}
	return manager
}

func (store serverStore) ChangedFiles(ctx context.Context, repository string, number int) ([]string, error) {
	return queue.ChangedFiles(ctx, store.Store, repository, number)
}
