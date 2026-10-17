package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

type fakeResumer struct {
	called int64
	err    error
}

func (s *fakeResumer) ResumeRegistration(_ context.Context, id int64) error {
	s.called = id
	return s.err
}

func TestResumeOnlyRequeuesExplicitLocalJob(t *testing.T) {
	var body bytes.Buffer
	s := &fakeResumer{}
	if err := resumeWithStore(context.Background(), s, 42, &body); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Queued      bool  `json:"queued"`
		OperationID int64 `json:"operation_id"`
	}
	if json.Unmarshal(body.Bytes(), &result) != nil || !result.Queued || result.OperationID != 42 || s.called != 42 {
		t.Fatal("wrong resume result")
	}
	for _, err := range []error{queue.ErrConflict, errors.New("never-print-private-database-detail")} {
		s := &fakeResumer{err: err}
		body.Reset()
		if got := resumeWithStore(context.Background(), s, 42, &body); got == nil || strings.Contains(got.Error(), "never-print") || body.Len() != 0 {
			t.Fatal("resume failure leaked details or reported success")
		}
	}
	if err := resumeWithStore(context.Background(), s, 42, failedReportWriter{}); err == nil || !strings.Contains(err.Error(), "requeued but report") {
		t.Fatal("lost report was not distinguished from requeue failure")
	}
}

func TestResumeRejectsInvalidOperationIDBeforeDatabaseAccess(t *testing.T) {
	t.Setenv("DATABASE_URL", "invalid-database-config")
	for _, id := range []string{"", "0", "-1", "not-a-number", "9223372036854775808"} {
		var body bytes.Buffer
		if err := resumeRegistration(context.Background(), id, &body); err == nil || !strings.Contains(err.Error(), "positive local operation ID") || body.Len() != 0 {
			t.Fatal("invalid operation reached database or produced a report")
		}
	}
}
