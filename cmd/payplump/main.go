package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/httpserver"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger, os.Args[1:]); err != nil {
		logger.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, args []string) error {
	if len(args) == 1 && args[0] == "healthcheck" {
		return healthcheck(envOr("HEALTHCHECK_URL", "http://127.0.0.1:8080/readyz"))
	}
	if len(args) != 0 {
		return errors.New("usage: payplump [healthcheck]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	maxConns, err := strconv.ParseInt(envOr("DB_MAX_CONNS", "4"), 10, 32)
	if err != nil {
		return errors.New("DB_MAX_CONNS must be an integer between 1 and 64")
	}
	startupCtx, cancelStartup := context.WithTimeout(ctx, 15*time.Second)
	defer cancelStartup()
	store, err := postgres.Open(startupCtx, os.Getenv("DATABASE_URL"), int32(maxConns))
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(startupCtx); err != nil {
		return fmt.Errorf("database migrations: %w", err)
	}
	cancelStartup()
	srv := httpserver.New(envOr("HTTP_ADDR", ":8080"), store.Ping)
	result := make(chan error, 1)
	go func() { result <- srv.ListenAndServe() }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
		if err := <-result; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func healthcheck(url string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return errors.New("healthcheck request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: HTTP %d", response.StatusCode)
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
