package main

import (
	"context"
	"fmt"
	"os"
	"time"

	httpapi "github.com/daltoniam/overload/http"
)

func installCheck() error {
	if err := httpapi.ValidateAuth(); err != nil {
		return err
	}
	if err := validateServeAddress(os.Getenv("OVERLOAD_ADDR"), os.Getenv("OVERLOAD_UI_INSECURE") == "1"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := database(ctx)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("database migrations: %w", err)
	}
	models, err := store.ListReviewSettings(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Database migrations: OK\nUI authentication and bind address: OK\nModel connections: %d (not probed)\n", len(models))
	return nil
}
