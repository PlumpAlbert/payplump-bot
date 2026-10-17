package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrations"
)

// This subcommand runs independently of Telegram and the bot database: no
// migrations, webhook setup, imports, invoice creation or access changes.
func checkIntegrations(ctx context.Context, output io.Writer) error {
	cfg, err := integrations.ConfigFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	report, checkErr := integrations.Check(ctx, cfg)
	if err := json.NewEncoder(output).Encode(report); err != nil {
		return errors.Join(checkErr, errors.New("could not write integration report"))
	}
	return checkErr
}
