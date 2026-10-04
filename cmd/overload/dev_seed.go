package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/daltoniam/overload/postgres"
)

func devSeed() error {
	if os.Getenv("OVERLOAD_DEV_SEED") != "1" {
		return errors.New("set OVERLOAD_DEV_SEED=1 to allow local demo data")
	}
	dsn := os.Getenv("DATABASE_URL")
	if err := postgres.IsLocalDatabase(dsn); err != nil {
		return err
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
	created, err := store.SeedLocalDemo(ctx)
	if err != nil {
		return err
	}
	if created {
		fmt.Println("Created disabled demo repositories, a disabled schedule, and example run output. No model or GitHub request was sent.")
	} else {
		fmt.Println("Demo data already exists; no changes made.")
	}
	return nil
}
