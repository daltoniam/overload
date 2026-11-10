package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/daltoniam/overload"
)

// userAdmin is what the users command needs from the store.
type userAdmin interface {
	ListUsers(context.Context) ([]overload.User, error)
	SetUserDisabled(ctx context.Context, id int64, disabled bool) error
}

func manageUsers(args []string) error {
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
	return runUsers(ctx, store, args, os.Stdout)
}

func runUsers(ctx context.Context, store userAdmin, args []string, out io.Writer) error {
	usage := errors.New("usage: overload users list | disable ID | enable ID")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usage
		}
		users, err := store.ListUsers(ctx)
		if err != nil {
			return err
		}
		writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "ID\tEMAIL\tROLE\tSTATUS\tLAST SIGN-IN")
		for _, user := range users {
			status, login := "active", "never"
			if user.Disabled {
				status = "disabled"
			}
			if user.LastLoginAt != nil {
				login = user.LastLoginAt.UTC().Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\n", user.ID, user.Email, user.Role, status, login)
		}
		return writer.Flush()
	case "disable", "enable":
		if len(args) != 2 {
			return usage
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id < 1 {
			return usage
		}
		if err := store.SetUserDisabled(ctx, id, args[0] == "disable"); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "User %d %sd\n", id, args[0])
		return err
	default:
		return usage
	}
}
