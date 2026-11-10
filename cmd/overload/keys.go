package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/daltoniam/overload"
)

// keyAdmin is what the keys command needs from the store.
type keyAdmin interface {
	CreateAPIKey(ctx context.Context, userID int64, name string, scopes []string, lifetime time.Duration) (overload.APIKey, string, error)
	ListAPIKeys(ctx context.Context, userID int64, all bool) ([]overload.APIKey, error)
	RevokeAPIKey(ctx context.Context, id, userID int64, anyOwner bool) error
}

func manageKeys(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	return runKeys(ctx, store, args, os.Stdout)
}

// runKeys manages shared API keys (keys owned by no person), for installs
// without OIDC sign-in and for automation; people create their own keys on
// the API keys page.
func runKeys(ctx context.Context, store keyAdmin, args []string, out io.Writer) error {
	usage := errors.New("usage: overload keys list | create --name NAME [--scopes models,reviews] [--days 90] | revoke ID")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usage
		}
		keys, err := store.ListAPIKeys(ctx, 0, true)
		if err != nil {
			return err
		}
		writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "ID\tNAME\tPREFIX\tOWNER\tSCOPES\tEXPIRES\tSTATUS")
		now := time.Now()
		for _, key := range keys {
			owner := key.UserEmail
			if owner == "" {
				owner = "shared"
			}
			_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", key.ID, key.Name, key.Prefix, owner, strings.Join(key.Scopes, ","), key.ExpiresAt.UTC().Format("2006-01-02"), key.State(now))
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		if len(keys) >= overload.APIKeyListLimit {
			_, err = fmt.Fprintf(out, "Showing the %d newest keys.\n", overload.APIKeyListLimit)
		}
		return err
	case "create":
		flags := flag.NewFlagSet("keys create", flag.ContinueOnError)
		name := flags.String("name", "", "what the key is for")
		days := flags.Int("days", 90, "days until the key expires (1 to 365)")
		scopes := flags.String("scopes", overload.ScopeModels, "comma-separated scopes: models, reviews")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return usage
		}
		key, secret, err := store.CreateAPIKey(ctx, 0, *name, parseScopes(*scopes), time.Duration(*days)*24*time.Hour)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Created key %d (%s), expiring %s. It is shown only once:\n%s\n", key.ID, key.Prefix, key.ExpiresAt.UTC().Format("2006-01-02"), secret)
		return err
	case "revoke":
		if len(args) != 2 {
			return usage
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id < 1 {
			return usage
		}
		if err := store.RevokeAPIKey(ctx, id, 0, true); errors.Is(err, overload.ErrInvalidAPIKey) {
			return fmt.Errorf("no active API key %d", id)
		} else if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Revoked key %d\n", id)
		return err
	default:
		return usage
	}
}

// parseScopes reads a comma-separated scope list, ignoring spaces, empty
// entries and repeats, so "models, reviews" means what it says.
func parseScopes(list string) []string {
	var scopes []string
	for _, scope := range strings.Split(list, ",") {
		if scope = strings.TrimSpace(scope); scope != "" && !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	return scopes
}
