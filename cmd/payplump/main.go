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

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/httpserver"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/outbox"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
	"github.com/PlumpAlbert/payplump-bot/internal/registration"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

func main() {
	logOutput := os.Stdout
	if len(os.Args) > 1 && (os.Args[1] == "check-integrations" || os.Args[1] == "resume-registration" || os.Args[1] == "recovery-report" || os.Args[1] == "import-plan" || os.Args[1] == "import-stage" || os.Args[1] == "import-verify" || os.Args[1] == "import-apply") {
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
	healthOnly := len(args) == 1 && args[0] == "serve-health"
	if len(args) > 0 && args[0] == "recovery-report" {
		return recoveryReportCommand(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "import-plan" {
		return importPlanCommand(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "import-stage" {
		return importStageCommand(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "import-verify" {
		return importVerifyCommand(args[1:], os.Stdout)
	}
	if len(args) > 0 && args[0] == "import-apply" {
		return importApplyCommand(args[1:], os.Stdout)
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
	if len(args) != 0 && !healthOnly {
		return errors.New("usage: payplump [serve-health | healthcheck | check-integrations | resume-registration OPERATION_ID | recovery-report [--operation LOCAL_ID] | import-plan --file PATH | import-stage --file PATH --expected-hash HASH --apply | import-verify --file PATH --expected-hash HASH --read-only [--evidence-file NEW_PRIVATE_PATH] | import-apply --file PRIVATE --expected-hash HASH --admission-revision N --apply [--enable-admission]]")
	}
	var get func(string) string
	var standard bool
	var err error
	if healthOnly {
		get, standard = healthEnvironment(os.Getenv), false
	} else {
		get, standard, err = serviceEnvironment(os.Getenv)
	}
	if err != nil {
		return err
	}
	telegramConfig, err := telegram.ConfigFromEnvironment(get)
	if err != nil {
		return err
	}
	broadcastConfig, err := broadcast.ConfigFromEnvironment(get, telegramConfig.Enabled, telegramConfig.AdminIDs)
	if err != nil {
		return err
	}
	registrationConfig, err := registration.ConfigFromEnvironment(get, telegramConfig.Enabled)
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
	paymentConfig, err := payments.ConfigFromEnvironment(get, telegramConfig.Enabled)
	if err != nil {
		return err
	}
	var paymentAPI *invoiceninja.SettlementClient
	if paymentConfig.Enabled {
		if len(telegramConfig.AdminIDs) > 99 {
			return errors.New("payment verification requires at most 99 administrators")
		}
		paymentAPI, err = invoiceninja.NewSettlementClient(paymentConfig.BaseURL, paymentConfig.Token, paymentConfig.HTTPTimeout, paymentConfig.PageSize, paymentConfig.MaxPages)
		if err != nil {
			return errors.New("invalid payment verification API configuration")
		}
	}
	lifecycleConfig, err := lifecycle.ConfigFromEnvironment(get, telegramConfig.Enabled, paymentConfig.Enabled, paymentConfig.WebhookSecret, telegramConfig.WebhookSecret, telegramConfig.Token, standard)
	if err != nil {
		return err
	}
	var lifecycleAPI *invoiceninja.CycleReader
	if lifecycleConfig.Enabled {
		lifecycleAPI, err = invoiceninja.NewCycleReader(lifecycleConfig.BaseURL, lifecycleConfig.Token, lifecycleConfig.HTTPTimeout, lifecycleConfig.PageSize, lifecycleConfig.MaxPages)
		if err != nil {
			return errors.New("invalid invoice lifecycle API configuration")
		}
	}
	enforcementConfig, err := lifecycle.EnforcementConfigFromEnvironment(get, lifecycleConfig.Enabled, telegramConfig.Enabled, paymentConfig.Enabled, standard)
	if err != nil {
		return err
	}
	renewalConfig, err := lifecycle.RenewalConfigFromEnvironment(get, lifecycleConfig.Enabled, telegramConfig.Enabled, paymentConfig.Enabled, standard)
	if err != nil {
		return err
	}
	var renewalRecurring lifecycle.RenewalRecurringWriter
	var renewalSUI lifecycle.RenewalSUIWriter
	if renewalConfig.Enabled {
		recurring, err := invoiceninja.NewRecurringClient(renewalConfig.BaseURL, renewalConfig.Token, renewalConfig.HTTPTimeout, 50, 20)
		if err != nil {
			return errors.New("invalid paid renewal recurring writer configuration")
		}
		panel, err := sui.NewManagedClient(renewalConfig.SUIBaseURL, renewalConfig.SUIToken, renewalConfig.SUIVersion, renewalConfig.HTTPTimeout, renewalConfig.MaxAccounts)
		if err != nil {
			return errors.New("invalid paid renewal S-UI writer configuration")
		}
		renewalRecurring, renewalSUI = recurring, panel
	}
	var enforcementRecurring *invoiceninja.RecurringClient
	var enforcementSUI *sui.ManagedClient
	if enforcementConfig.Enabled {
		enforcementRecurring, err = invoiceninja.NewRecurringClient(enforcementConfig.BaseURL, enforcementConfig.Token, enforcementConfig.HTTPTimeout, 50, 20)
		if err != nil {
			return errors.New("invalid invoice overdue recurring writer configuration")
		}
		enforcementSUI, err = sui.NewManagedClient(enforcementConfig.SUIBaseURL, enforcementConfig.SUIToken, enforcementConfig.SUIVersion, enforcementConfig.HTTPTimeout, enforcementConfig.MaxAccounts)
		if err != nil {
			return errors.New("invalid invoice overdue S-UI writer configuration")
		}
	}
	activationConfig, err := activation.ConfigFromEnvironment(get, telegramConfig.Enabled, paymentConfig.Enabled, standard)
	if err != nil {
		return err
	}
	var activationAPIs activation.Providers
	if activationConfig.Enabled {
		if len(telegramConfig.AdminIDs) > 99 {
			return errors.New("activation requires at most 99 administrators")
		}
		activationAPIs, err = newActivationProviders(activationConfig)
		if err != nil {
			return err
		}
	}
	configurationConfig, err := configdelivery.ConfigFromEnvironment(get, telegramConfig.Enabled, paymentConfig.Enabled, standard)
	if err != nil {
		return err
	}
	configurationAPIs, err := newConfigurationProviders(configurationConfig)
	if err != nil {
		return err
	}
	var importedRuntimeConfig importedRuntimeConfig
	if standard {
		importedRuntimeConfig, err = serviceImportedRuntimeConfig(enforcementConfig)
	} else {
		importedRuntimeConfig, err = loadImportedRuntimeConfig(get, telegramConfig.Enabled, paymentConfig.Enabled)
	}
	if err != nil {
		return err
	}
	billingProviders, err := newImportedBillingProviders(importedRuntimeConfig, enforcementConfig, renewalConfig)
	if err != nil {
		return err
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
	if !broadcastConfig.Enabled {
		if err := store.DisableBroadcastPolicies(startupCtx); err != nil {
			return fmt.Errorf("broadcast startup fence: %w", err)
		}
	}
	if err := fenceConfigurationStartup(startupCtx, store, configurationConfig); err != nil {
		return err
	}
	if !standard {
		if err := fenceImportedRuntimeStartup(startupCtx, store, importedRuntimeConfig); err != nil {
			return err
		}
	}
	cancelStartup()
	var paymentEndpoint *payments.WebhookHandler
	var paymentHandler http.Handler
	if paymentConfig.Enabled {
		paymentEndpoint, err = payments.NewWebhook(paymentConfig.WebhookSecret, store)
		if err != nil {
			return err
		}
		paymentHandler = paymentEndpoint
	}
	var lifecycleEndpoint *lifecycle.WebhookHandler
	var lifecycleHandler http.Handler
	if lifecycleConfig.Enabled {
		lifecycleEndpoint, err = lifecycle.NewWebhook(lifecycleConfig.WebhookSecret, store)
		if err != nil {
			return err
		}
		lifecycleHandler = lifecycleEndpoint
	}
	var endpoint *telegram.WebhookHandler
	var handler http.Handler
	var initialize func(context.Context) error
	var background func(context.Context) error
	if telegramConfig.Enabled {
		gateway := &startupGateway{}
		endpoint, err = telegram.NewWebhookHandler(telegramConfig.WebhookSecret, gateway)
		if err != nil {
			return err
		}
		handler = endpoint
		client := telegram.NewClient(telegramConfig.Token)
		var botID int64
		var importer *automaticImporter
		initialize = func(ctx context.Context) error {
			bot, err := telegram.ConfigureWebhook(ctx, client, telegramConfig, logger)
			if err != nil {
				return err
			}
			botID = bot.ID
			if standard {
				bp, cp := broadcastConfig.Policy(), configurationConfig.Policy()
				if err := store.InstallServiceBroadcastPolicy(ctx, botID, &bp); err != nil {
					return err
				}
				if err := store.InstallServiceConfigurationPolicy(ctx, botID, &cp); err != nil {
					return err
				}
				broadcastConfig.Revision, configurationConfig.Revision = bp.Revision, cp.Revision
				readCtx, stop := context.WithTimeout(ctx, 15*time.Second)
				verification, err := serviceVerificationConfig(readCtx, get, botID, activationConfig)
				stop()
				if err != nil {
					return err
				}
				providers, err := newVerifyProviders(verification)
				if err != nil {
					return err
				}
				inventory, err := invoiceninja.NewCustomerInventoryReader(verification.ninjaBase, verification.ninjaToken, verification.timeout, verification.pageSize, verification.maxPages)
				if err != nil {
					return err
				}
				providers.Customers = inventory
				verifier, err := importverify.New(verification.policy, providers)
				if err != nil {
					return err
				}
				policyHash, err := verification.policy.Fingerprint()
				if err != nil {
					return err
				}
				admission, read, effects, err := store.InstallServiceImportPolicies(ctx, botID, policyHash, verification.policy.SourceScopeHash)
				if err != nil {
					return err
				}
				importedRuntimeConfig.BotID, importedRuntimeConfig.PolicyHash = botID, policyHash
				importedRuntimeConfig.Revision, importedRuntimeConfig.EffectsRevision = read, effects
				admins := make([]int64, 0, len(telegramConfig.AdminIDs))
				for id := range telegramConfig.AdminIDs {
					admins = append(admins, id)
				}
				importer = &automaticImporter{store: store, verifier: verifier, policy: verification.policy, revision: admission, admins: admins, logger: logger, now: time.Now}
				scanCtx, stop := context.WithTimeout(ctx, 3*time.Minute)
				err = importer.Scan(scanCtx)
				stop()
				if err != nil {
					return err
				}
			} else {
				if broadcastConfig.Enabled {
					if err := store.InstallBroadcastPolicy(ctx, botID, broadcastConfig.Policy()); err != nil {
						return err
					}
				}
				if err := store.InstallConfigurationPolicy(ctx, botID, configurationConfig.Policy()); err != nil {
					return err
				}
				if err := installImportedRuntimePermission(ctx, store, botID, importedRuntimeConfig); err != nil {
					return err
				}
				if err := installImportedEffectPermission(ctx, store, botID, importedRuntimeConfig); err != nil {
					return err
				}
			}
			menu, err := newMenuGatewayWithConfiguration(store, configurationConfig)
			if err != nil {
				return err
			}
			gateway.install(newBroadcastGateway(menu, store, broadcastConfig))
			if paymentEndpoint != nil {
				if err := paymentEndpoint.Activate(botID); err != nil {
					return err
				}
			}
			if lifecycleEndpoint != nil {
				if err := lifecycleEndpoint.Activate(botID); err != nil {
					return err
				}
			}
			return endpoint.Activate(bot)
		}
		background = func(ctx context.Context) error {
			admins := make([]int64, 0, len(telegramConfig.AdminIDs))
			for id := range telegramConfig.AdminIDs {
				admins = append(admins, id)
			}
			configuration, err := configdelivery.New(store, configurationAPIs, client, botID, configurationConfig, logger)
			if err != nil {
				return err
			}
			sender := &configurationGateway{Client: client, delivery: configuration}
			worker, err := outbox.New(store, sender, botID, deliveryConfig, logger)
			if err != nil {
				return err
			}
			workers := []func(context.Context) error{worker.Run}
			if importer != nil {
				workers = append(workers, importer.Run)
			}
			if broadcastConfig.Enabled {
				broadcastWorker, err := broadcast.New(store, botID, broadcastConfig, logger)
				if err != nil {
					return err
				}
				workers = append(workers, broadcastWorker.Run)
			}
			if registrationConfig.Enabled {
				onboarding, err := registration.New(store, registrationAPI, botID, admins, registrationConfig, logger)
				if err != nil {
					return err
				}
				workers = append(workers, onboarding.Run)
			}
			if paymentConfig.Enabled {
				verifier, err := payments.New(store, paymentAPI, botID, admins, paymentConfig, logger)
				if err != nil {
					return err
				}
				workers = append(workers, verifier.Run)
			}
			if lifecycleConfig.Enabled {
				worker, err := lifecycle.New(store, lifecycleAPI, botID, admins, lifecycleConfig, logger)
				if err != nil {
					return err
				}
				workers = append(workers, worker.Run)
			}
			if enforcementConfig.Enabled {
				var worker *lifecycle.EnforcementWorker
				if billingProviders.Enforcement != nil {
					worker, err = lifecycle.NewEnforcementWorkerWithImported(store, lifecycleAPI, enforcementRecurring, billingProviders.Enforcement, enforcementSUI, botID, admins, enforcementConfig, logger)
				} else {
					worker, err = lifecycle.NewEnforcementWorker(store, lifecycleAPI, enforcementRecurring, enforcementSUI, botID, admins, enforcementConfig, logger)
				}
				if err != nil {
					return err
				}
				workers = append(workers, worker.Run)
			}
			if renewalConfig.Enabled {
				var worker *lifecycle.RenewalWorker
				if billingProviders.Renewal != nil {
					worker, err = lifecycle.NewRenewalWorkerWithImported(store, lifecycleAPI, paymentAPI, renewalRecurring, billingProviders.Renewal, renewalSUI, botID, admins, paymentConfig.CurrencyID, renewalConfig, logger)
				} else {
					worker, err = lifecycle.NewRenewalWorker(store, lifecycleAPI, paymentAPI, renewalRecurring, renewalSUI, botID, admins, paymentConfig.CurrencyID, renewalConfig, logger)
				}
				if err != nil {
					return err
				}
				workers = append(workers, worker.Run)
			}
			if activationConfig.Enabled {
				provisioner, err := activation.New(store, activationAPIs, botID, admins, activationConfig, logger)
				if err != nil {
					return err
				}
				workers = append(workers, provisioner.Run)
			}
			return runWorkers(ctx, workers...)
		}
	}
	ready := func(ctx context.Context) error {
		if endpoint != nil && !endpoint.Ready() {
			return errors.New("Telegram webhook setup pending")
		}
		if lifecycleEndpoint != nil && !lifecycleEndpoint.Ready() {
			return errors.New("invoice lifecycle webhook setup pending")
		}
		return store.Ping(ctx)
	}
	srv := httpserver.New(envOr("HTTP_ADDR", ":8080"), ready, handler, paymentHandler, lifecycleHandler)
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
