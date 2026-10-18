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
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/outbox"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
	"github.com/PlumpAlbert/payplump-bot/internal/registration"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

func main() {
	logOutput := os.Stdout
	if len(os.Args) > 1 && (os.Args[1] == "check-integrations" || os.Args[1] == "resume-registration") {
		logOutput = os.Stderr // Keep the command's stdout a single JSON report.
	}
	logger := slog.New(slog.NewJSONHandler(logOutput, nil))
	if err := run(logger, os.Args[1:]); err != nil {
		logger.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, args []string) error {
	if len(args) == 1 && args[0] == "healthcheck" {
		return healthcheck(envOr("HEALTHCHECK_URL", "http://127.0.0.1:8080/readyz"))
	}
	if len(args) == 1 && args[0] == "check-integrations" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return checkIntegrations(ctx, os.Stdout)
	}
	if len(args) == 2 && args[0] == "resume-registration" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return resumeRegistration(ctx, args[1], os.Stdout)
	}
	if len(args) != 0 {
		return errors.New("usage: payplump [healthcheck | check-integrations | resume-registration OPERATION_ID]")
	}
	telegramConfig, err := telegram.ConfigFromEnv()
	if err != nil {
		return err
	}
	registrationConfig, err := registration.ConfigFromEnv(telegramConfig.Enabled)
	if err != nil {
		return err
	}
	var registrationAPI *invoiceninja.BillingClient
	if registrationConfig.Enabled {
		if len(telegramConfig.AdminIDs) > 99 {
			return errors.New("registration requires at most 99 Telegram administrators")
		}
		registrationAPI, err = invoiceninja.NewBillingClient(registrationConfig.BaseURL, registrationConfig.Token, registrationConfig.HTTPTimeout, registrationConfig.PageSize, registrationConfig.MaxPages)
		if err != nil {
			return errors.New("invalid registration API configuration")
		}
	}
	var deliveryConfig outbox.Config
	if telegramConfig.Enabled {
		deliveryConfig, err = outbox.ConfigFromEnv()
		if err != nil {
			return err
		}
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
	var endpoint *telegram.WebhookHandler
	var handler http.Handler
	var initialize func(context.Context) error
	var background func(context.Context) error
	if telegramConfig.Enabled {
		gateway, err := newMenuGateway(store)
		if err != nil {
			return err
		}
		endpoint, err = telegram.NewWebhookHandler(telegramConfig.WebhookSecret, gateway)
		if err != nil {
			return err
		}
		handler = endpoint
		client := telegram.NewClient(telegramConfig.Token)
		var botID int64
		initialize = func(ctx context.Context) error {
			bot, err := telegram.ConfigureWebhook(ctx, client, telegramConfig, logger)
			if err != nil {
				return err
			}
			botID = bot.ID
			return endpoint.Activate(bot)
		}
		background = func(ctx context.Context) error {
			worker, err := outbox.New(store, client, botID, deliveryConfig, logger)
			if err != nil {
				return err
			}
			workers := []func(context.Context) error{worker.Run}
			if registrationConfig.Enabled {
				var admins []int64
				for id := range telegramConfig.AdminIDs {
					admins = append(admins, id)
				}
				onboarding, err := registration.New(store, registrationAPI, botID, admins, registrationConfig, logger)
				if err != nil {
					return err
				}
				workers = append(workers, onboarding.Run)
			}
			return runWorkers(ctx, workers...)
		}
	}
	ready := func(ctx context.Context) error {
		if endpoint != nil && !endpoint.Ready() {
			return errors.New("Telegram webhook setup pending")
		}
		return store.Ping(ctx)
	}
	srv := httpserver.New(envOr("HTTP_ADDR", ":8080"), ready, handler)
	return serve(ctx, logger, srv, initialize, background)
}

// The listener is opened before registering the webhook. Successful setup leaves
// HTTP serving; fatal setup errors stop the service. Shutdown drains in-flight
// requests and joins background deliveries before the database pool is closed.
// Telegram's webhook and interrupted durable leases are preserved.
func serve(ctx context.Context, logger *slog.Logger, srv *http.Server, initialize, background func(context.Context) error) error {
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("HTTP listen: %w", err)
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- srv.Serve(listener) }()
	var setupResult, backgroundResult chan error
	startBackground := func() {
		if background != nil {
			backgroundResult = make(chan error, 1)
			go func() { backgroundResult <- background(serviceCtx) }()
		}
	}
	if initialize != nil {
		setupResult = make(chan error, 1)
		go func() { setupResult <- initialize(serviceCtx) }()
	} else {
		startBackground()
	}
	var failure error
running:
	for {
		select {
		case err := <-result:
			result = nil
			if !errors.Is(err, http.ErrServerClosed) {
				failure = err
			}
			break running
		case err := <-setupResult:
			setupResult = nil
			if err == nil {
				startBackground()
				continue
			}
			if ctx.Err() == nil {
				failure = fmt.Errorf("Telegram webhook setup: %w", err)
			}
			break running
		case err := <-backgroundResult:
			backgroundResult = nil
			if ctx.Err() == nil {
				if err == nil {
					err = errors.New("worker stopped unexpectedly")
				}
				failure = fmt.Errorf("background workers: %w", err)
			}
			break running
		case <-ctx.Done():
			break running
		}
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
	if setupResult != nil {
		select {
		case err := <-setupResult:
			if !errors.Is(err, context.Canceled) {
				failure = errors.Join(failure, err)
			}
		case <-shutdownCtx.Done():
			failure = errors.Join(failure, errors.New("Telegram webhook setup shutdown timed out"))
		}
	}
	if backgroundResult != nil {
		select {
		case err := <-backgroundResult:
			if err != nil && !errors.Is(err, context.Canceled) {
				failure = errors.Join(failure, err)
			}
		case <-shutdownCtx.Done():
			failure = errors.Join(failure, errors.New("background workers shutdown timed out"))
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
