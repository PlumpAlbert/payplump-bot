package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/httpserver"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
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
	telegramConfig, err := telegram.ConfigFromEnv()
	if err != nil {
		return err
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
	var worker func(context.Context) error
	if telegramConfig.Enabled {
		worker = telegram.NewPoller(telegram.NewClient(telegramConfig.Token), store, logger).Run
	}
	return serve(ctx, logger, srv, worker)
}

// A fatal worker error stops HTTP too; stopping HTTP cancels the poller before
// waiting for it. Both components finish before the database pool is closed.
func serve(ctx context.Context, logger *slog.Logger, srv *http.Server, worker func(context.Context) error) error {
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("HTTP listen: %w", err)
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- srv.Serve(listener) }()
	var workerResult chan error
	if worker != nil {
		workerResult = make(chan error, 1)
		go func() { workerResult <- worker(serviceCtx) }()
	}
	var failure error
	select {
	case err := <-result:
		result = nil
		if !errors.Is(err, http.ErrServerClosed) {
			failure = err
		}
	case err := <-workerResult:
		workerResult = nil
		if err != nil {
			failure = fmt.Errorf("Telegram polling: %w", err)
		} else if ctx.Err() == nil {
			failure = errors.New("Telegram polling stopped unexpectedly")
		}
	case <-ctx.Done():
	}
	cancel()
	logger.Info("shutting down")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		failure = errors.Join(failure, fmt.Errorf("HTTP shutdown: %w", err))
	}
	if result != nil {
		if err := <-result; !errors.Is(err, http.ErrServerClosed) {
			failure = errors.Join(failure, err)
		}
	}
	if workerResult != nil {
		select {
		case err := <-workerResult:
			failure = errors.Join(failure, err)
		case <-shutdownCtx.Done():
			failure = errors.Join(failure, errors.New("Telegram shutdown timed out"))
		}
	}
	return failure
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
