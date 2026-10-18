package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

func importPlanCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("import-plan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("file", "", "private manifest path")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *path == "" {
		return errors.New("usage: payplump import-plan --file PATH")
	}
	m, err := loadPrivateManifest(*path)
	if err != nil {
		return err
	}
	p := importplan.Analyze(m)
	_, err = fmt.Fprintln(out, string(importplan.SafeSummary(p)))
	return err
}

type importStager interface {
	StageImport(context.Context, importplan.Plan, string) (bool, error)
	Close()
}

func importStageCommand(args []string, out io.Writer) error {
	return importStageWith(args, out, func(ctx context.Context, url string, max int32) (importStager, error) {
		return postgres.Open(ctx, url, max)
	})
}

func importStageWith(args []string, out io.Writer, open func(context.Context, string, int32) (importStager, error)) error {
	fs := flag.NewFlagSet("import-stage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("file", "", "private manifest path")
	expected := fs.String("expected-hash", "", "plan hash")
	apply := fs.Bool("apply", false, "stage immutable candidates")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *path == "" || *expected == "" || !*apply {
		return errors.New("usage: payplump import-stage --file PATH --expected-hash HASH --apply")
	}
	m, err := loadPrivateManifest(*path)
	if err != nil {
		return err
	}
	p := importplan.Analyze(m)
	if err = p.ValidateHash(*expected); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	max := int64(4)
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		max, err = strconv.ParseInt(v, 10, 32)
		if err != nil {
			return errors.New("invalid DB_MAX_CONNS")
		}
	}
	store, err := open(ctx, os.Getenv("DATABASE_URL"), int32(max))
	if err != nil {
		return err
	}
	defer store.Close()
	repeated, err := store.StageImport(ctx, p, *expected)
	if err != nil {
		return err
	}
	state := "staged"
	if repeated {
		state = "already_staged"
	}
	_, err = fmt.Fprintf(out, "{\"plan_hash\":%q,\"candidates\":%d,\"state\":%q}\n", p.Hash, len(p.Candidates), state)
	return err
}
func loadPrivateManifest(path string) (importplan.Manifest, error) {
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Mode().Perm()&0077 != 0 {
		return importplan.Manifest{}, errors.New("manifest must be a regular private file with mode 0600")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return importplan.Manifest{}, errors.New("cannot open manifest")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0600 {
		return importplan.Manifest{}, errors.New("manifest must be a regular private file with mode 0600")
	}
	m, err := importplan.Decode(f)
	if err != nil {
		return importplan.Manifest{}, err
	}
	return m, nil
}
