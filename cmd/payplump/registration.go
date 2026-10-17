package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

type registrationResumer interface {
	ResumeRegistration(context.Context, int64) error
}

// Explicit operator action: requeue a held local job, never clear its journal
// or resend a request here. This command neither migrates nor calls an API.
func resumeRegistration(ctx context.Context, rawID string, output io.Writer) error {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		return errors.New("resume-registration requires a positive local operation ID")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	store, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"), 1)
	if err != nil {
		return err
	}
	defer store.Close()
	return resumeWithStore(ctx, store, id, output)
}

func resumeWithStore(ctx context.Context, store registrationResumer, id int64, output io.Writer) error {
	if id <= 0 {
		return errors.New("resume-registration requires a positive local operation ID")
	}
	if err := store.ResumeRegistration(ctx, id); err != nil {
		return errors.New("could not requeue registration; verify the held state and installed migrations")
	}
	if err := json.NewEncoder(output).Encode(struct {
		Queued      bool  `json:"queued"`
		OperationID int64 `json:"operation_id"`
	}{true, id}); err != nil {
		return errors.New("registration requeued but report could not be written; inspect state before repeating")
	}
	return nil
}
