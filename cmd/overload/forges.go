package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/forge"
	httpapi "github.com/daltoniam/overload/http"
	"github.com/daltoniam/overload/postgres"
)

const forgesUsage = "usage: overload forges list | apply FILE|- | secret NAME | webhook NAME REPOSITORY | delete NAME"

// forgeInput is the JSON overload forges apply reads. The token comes from
// the environment variable token_env names, so it stays out of files and
// shell history.
type forgeInput struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	BaseURL  string `json:"base_url"`
	TokenEnv string `json:"token_env,omitempty"`
}

// manageForges handles GitLab and Forgejo connections.
func manageForges(args []string) error {
	if len(args) == 0 {
		return errors.New(forgesUsage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	return runForgeCommand(ctx, store, args, os.Stdin, os.Stdout, forge.New)
}

func runForgeCommand(ctx context.Context, store *postgres.Store, args []string, stdin io.Reader, stdout io.Writer, newClient func(overload.ForgeConnection) (forge.Client, error)) error {
	switch {
	case args[0] == "list" && len(args) == 1:
		connections, err := store.ListForges(ctx)
		if err != nil {
			return err
		}
		type listed struct {
			Name        string `json:"name"`
			Kind        string `json:"kind"`
			BaseURL     string `json:"base_url"`
			WebhookPath string `json:"webhook_path"`
		}
		out := make([]listed, 0, len(connections))
		for _, connection := range connections {
			out = append(out, listed{Name: connection.Name, Kind: connection.Kind, BaseURL: connection.BaseURL, WebhookPath: httpapi.ForgeWebhookPath(connection)})
		}
		return json.NewEncoder(stdout).Encode(out)
	case args[0] == "apply" && len(args) == 2:
		reader := stdin
		if args[1] != "-" {
			file, err := os.Open(args[1])
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
			reader = file
		}
		data, err := readConfiguration(reader)
		if err != nil {
			return err
		}
		var input forgeInput
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return fmt.Errorf("invalid git host JSON: %w", err)
		}
		connection := overload.ForgeConnection{Name: input.Name, Kind: input.Kind, BaseURL: input.BaseURL}
		check := connection
		if input.TokenEnv != "" {
			connection.Token = os.Getenv(input.TokenEnv)
			if connection.Token == "" {
				return fmt.Errorf("%s is empty", input.TokenEnv)
			}
			check.Token = connection.Token
		} else if existing, err := store.LoadForgeByName(ctx, input.Name); err == nil {
			if check.Token, err = overload.ReusableToken(existing, check); err != nil {
				return fmt.Errorf(`%w: set "token_env"`, err)
			}
		} else {
			return errors.New(`set "token_env" to the environment variable holding the access token`)
		}
		if err := check.Validate(); err != nil {
			return err
		}
		client, err := newClient(check)
		if err != nil {
			return err
		}
		identity, err := client.Identity(ctx)
		if err != nil {
			return fmt.Errorf("%s did not accept the token: %w", check.BaseURL, err)
		}
		saved, err := store.SaveForge(ctx, connection)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]string{"name": saved.Name, "kind": saved.Kind, "base_url": saved.BaseURL, "account": identity, "webhook_path": httpapi.ForgeWebhookPath(saved)})
	case args[0] == "secret" && len(args) == 2:
		connection, err := store.LoadForgeByName(ctx, args[1])
		if err != nil {
			return fmt.Errorf("git host %q: %w", args[1], err)
		}
		_, err = fmt.Fprintln(stdout, connection.WebhookSecret)
		return err
	case args[0] == "webhook" && len(args) == 3:
		connection, err := store.LoadForgeByName(ctx, args[1])
		if err != nil {
			return fmt.Errorf("git host %q: %w", args[1], err)
		}
		base := strings.TrimRight(os.Getenv("OVERLOAD_BASE_URL"), "/")
		if !strings.HasPrefix(base, "https://") {
			return errors.New("set OVERLOAD_BASE_URL to overload's public https address")
		}
		client, err := newClient(connection)
		if err != nil {
			return err
		}
		url := base + httpapi.ForgeWebhookPath(connection)
		if err := client.CreateWebhook(ctx, args[2], url, connection.WebhookSecret); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Webhook for %s sends pull request events to %s\n", args[2], url)
		return err
	case args[0] == "delete" && len(args) == 2:
		connection, err := store.LoadForgeByName(ctx, args[1])
		if err != nil {
			return fmt.Errorf("git host %q: %w", args[1], err)
		}
		return store.DeleteForge(ctx, connection.ID)
	}
	return errors.New(forgesUsage)
}
