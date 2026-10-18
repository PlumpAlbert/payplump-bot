package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

type recoveryReader interface {
	RecoverySnapshot(context.Context, int64) (postgres.RecoveryReport, error)
	Close()
}

func recoveryReportCommand(args []string, out io.Writer) error {
	return recoveryReportWith(args, out, func(ctx context.Context, url string, max int32) (recoveryReader, error) {
		return postgres.Open(ctx, url, max)
	})
}

func recoveryReportWith(args []string, out io.Writer, open func(context.Context, string, int32) (recoveryReader, error)) error {
	fs := flag.NewFlagSet("recovery-report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	local := fs.String("operation", "", "local operation id")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return errors.New("usage: payplump recovery-report [--operation LOCAL_ID]")
	}
	var id int64
	var err error
	if *local != "" {
		id, err = strconv.ParseInt(*local, 10, 64)
		if err != nil || id < 1 {
			return errors.New("operation must be a positive local id")
		}
	}
	max := int64(4)
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		max, err = strconv.ParseInt(v, 10, 32)
		if err != nil {
			return errors.New("invalid DB_MAX_CONNS")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := open(ctx, os.Getenv("DATABASE_URL"), int32(max))
	if err != nil {
		return err
	}
	defer store.Close()
	report, err := store.RecoverySnapshot(ctx, id)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(true)
	return enc.Encode(report)
}
