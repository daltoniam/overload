package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/daltoniam/overload"
)

type fakeUsers struct {
	users []overload.User
}

func (store *fakeUsers) ListUsers(context.Context) ([]overload.User, error) { return store.users, nil }
func (store *fakeUsers) SetUserDisabled(_ context.Context, id int64, disabled bool) error {
	store.users[id-1].Disabled = disabled
	return nil
}

func TestRunUsers(t *testing.T) {
	store := &fakeUsers{users: []overload.User{{ID: 1, Email: "a@example.com", Role: overload.RoleAdmin}}}
	var out bytes.Buffer
	ctx := context.Background()
	if err := runUsers(ctx, store, []string{"disable", "1"}, &out); err != nil || !store.users[0].Disabled {
		t.Fatalf("disable: %v", err)
	}
	out.Reset()
	if err := runUsers(ctx, store, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "a@example.com") || !strings.Contains(out.String(), "disabled") {
		t.Fatalf("list: %v %s", err, out.String())
	}
	for _, args := range [][]string{{}, {"disable"}, {"disable", "x"}, {"remove", "1"}} {
		if err := runUsers(ctx, store, args, &out); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
