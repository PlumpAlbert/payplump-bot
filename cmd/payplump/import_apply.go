package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

type importApplyStore interface {
	SyncImportAdmissionPolicy(context.Context, int64, int64, string, string, bool) error
	ApplyVerifiedImport(context.Context, importadmission.PreparedPlan) (postgres.ImportAdmissionResult, error)
	Close()
}

type importApplyOpener func(context.Context, string, int32) (importApplyStore, error)

func importApplyCommand(args []string, out io.Writer) error {
	return importApplyWith(args, out, os.Getenv, newVerifyProviders, time.Now, func(ctx context.Context, url string, max int32) (importApplyStore, error) {
		return postgres.Open(ctx, url, max)
	})
}

func importApplyWith(args []string, out io.Writer, getenv verifyEnvironment, factory verifyFactory, clock verifyClock, open importApplyOpener) error {
	fs := flag.NewFlagSet("import-apply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("file", "", "private manifest")
	expected := fs.String("expected-hash", "", "plan hash")
	revisionRaw := fs.String("admission-revision", "", "admission revision")
	apply := fs.Bool("apply", false, "apply verified import")
	enable := fs.Bool("enable-admission", false, "explicitly enable admission")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *path == "" || *expected == "" || !*apply {
		return errors.New("usage: payplump import-apply --file PRIVATE --expected-hash HASH --admission-revision N --apply [--enable-admission]")
	}
	revision, err := strconv.ParseInt(*revisionRaw, 10, 64)
	if err != nil || revision <= 0 || strconv.FormatInt(revision, 10) != *revisionRaw {
		return errors.New("invalid import admission revision")
	}
	manifest, err := loadPrivateManifest(*path)
	if err != nil {
		return errors.New("private manifest unavailable")
	}
	plan := importplan.Analyze(manifest)
	if plan.ValidateHash(*expected) != nil || plan.Hash != *expected || plan.Total == 0 || plan.Total != len(plan.Candidates) {
		return errors.New("invalid import apply plan")
	}
	cfg, err := loadVerifyConfig(getenv)
	if err != nil {
		return errors.New("invalid import verification configuration")
	}
	maxConns := int64(4)
	if raw := getenv("DB_MAX_CONNS"); raw != "" {
		maxConns, err = strconv.ParseInt(raw, 10, 32)
		if err != nil || maxConns < 1 || maxConns > 64 || strconv.FormatInt(maxConns, 10) != raw {
			return errors.New("invalid import apply database configuration")
		}
	}
	location, err := loadVerifyLocation(getenv("TZ"))
	if err != nil {
		return errors.New("invalid import verification configuration")
	}
	providers, err := factory(cfg)
	if err != nil {
		return errors.New("import verification sources unavailable")
	}
	now := clock()
	if now.IsZero() {
		return errors.New("invalid import verification time")
	}
	service, err := importverify.New(cfg.policy, providers)
	if err != nil {
		return errors.New("invalid import verification policy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	prepared, err := importadmission.Prepare(ctx, service, plan, *expected, revision, now.In(location))
	if err != nil || prepared.Validate() != nil {
		return errors.New("fresh import verification failed")
	}
	store, err := open(ctx, getenv("DATABASE_URL"), int32(maxConns))
	if err != nil {
		return errors.New("import apply database unavailable")
	}
	defer store.Close()
	if *enable {
		if err := store.SyncImportAdmissionPolicy(ctx, prepared.BotID(), prepared.Revision(), prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
			return errors.New("import admission policy update failed")
		}
	}
	result, err := store.ApplyVerifiedImport(ctx, prepared)
	if err != nil {
		return errors.New("verified import apply failed")
	}
	_, err = fmt.Fprintf(out, "{\"plan_hash\":%q,\"admitted\":%d,\"already_admitted\":%d,\"state\":\"applied\"}\n", prepared.PlanHash(), result.Admitted, result.AlreadyAdmitted)
	if err != nil {
		return errors.New("import apply result unavailable")
	}
	return nil
}
