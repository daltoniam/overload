package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/forge"
	"github.com/daltoniam/overload/postgres"
)

type cliForge struct {
	forge.Client
	token string
	hooks []string
}

func (client *cliForge) Identity(context.Context) (string, error) {
	if client.token != "glpat-good" {
		return "", errors.New("HTTP 401")
	}
	return "overload-bot", nil
}

func (client *cliForge) CreateWebhook(_ context.Context, repository, url, secret string) error {
	client.hooks = append(client.hooks, repository+" "+url+" "+secret)
	return nil
}

func TestForgeCommands(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL required")
	}
	ctx := context.Background()
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Pool.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("cli-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = store.Pool.Exec(ctx, `DELETE FROM forge_connections WHERE name=$1`, name) })
	stub := &cliForge{}
	newClient := func(connection overload.ForgeConnection) (forge.Client, error) {
		stub.token = connection.Token
		return stub, nil
	}
	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := runForgeCommand(ctx, store, args, strings.NewReader(stdin), &out, newClient)
		return out.String(), err
	}
	input := `{"name":"` + name + `","kind":"gitlab","base_url":"https://gitlab.example.com","token_env":"TEST_GITLAB_TOKEN"}`
	t.Setenv("TEST_GITLAB_TOKEN", "glpat-bad")
	if _, err := run(input, "apply", "-"); err == nil || !strings.Contains(err.Error(), "did not accept") {
		t.Fatalf("bad token: %v", err)
	}
	t.Setenv("TEST_GITLAB_TOKEN", "glpat-good")
	out, err := run(input, "apply", "-")
	if err != nil || !strings.Contains(out, `"account":"overload-bot"`) || strings.Contains(out, "glpat") {
		t.Fatalf("apply: %q %v", out, err)
	}
	out, err = run("", "list")
	var listed []map[string]string
	if err != nil || json.Unmarshal([]byte(out), &listed) != nil || strings.Contains(out, "glpat") {
		t.Fatalf("list: %q %v", out, err)
	}
	secret, err := run("", "secret", name)
	if err != nil || len(strings.TrimSpace(secret)) != 64 {
		t.Fatalf("secret: %q %v", secret, err)
	}
	t.Setenv("OVERLOAD_BASE_URL", "https://overload.example/")
	if _, err := run("", "webhook", name, "group/sub/api"); err != nil {
		t.Fatal(err)
	}
	if len(stub.hooks) != 1 || !strings.HasPrefix(stub.hooks[0], "group/sub/api https://overload.example/webhooks/gitlab/") || !strings.HasSuffix(stub.hooks[0], strings.TrimSpace(secret)) {
		t.Fatalf("hooks = %v", stub.hooks)
	}
	if _, err := run("", "delete", name); err != nil {
		t.Fatal(err)
	}
	if _, err := run("", "secret", name); err == nil {
		t.Fatal("deleted connection still has a secret")
	}
}
