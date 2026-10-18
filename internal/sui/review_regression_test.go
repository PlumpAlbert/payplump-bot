package sui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func reviewUUID(t *testing.T, r ClientRecord) string {
	t.Helper()
	var cfg map[string]map[string]any
	if json.Unmarshal(r.wire.Config, &cfg) != nil {
		t.Fatal("invalid synthetic config")
	}
	uuid, ok := cfg["vless"]["uuid"].(string)
	if !ok {
		t.Fatal("synthetic UUID missing")
	}
	return uuid
}

func reviewAliases(uuid string) map[string]string {
	return map[string]string{"canonical": uuid, "uppercase": strings.ToUpper(uuid), "compact": strings.ReplaceAll(uuid, "-", ""), "braced": "{" + uuid + "}", "urn": "urn:uuid:" + uuid}
}

func reviewForeignCredential(t *testing.T, alias string) ClientRecord {
	t.Helper()
	r := managedTestRecord(t, 124)
	r.wire.Config, _ = json.Marshal(map[string]any{"vless": map[string]any{"name": r.Name(), "uuid": alias}})
	return r
}

func TestManagedUUIDAliasesBlockCreationAndRecovery(t *testing.T) {
	d, err := NewDisabledDraft([]int64{7})
	if err != nil {
		t.Fatal("draft generation failed")
	}
	for name, alias := range reviewAliases(reviewUUID(t, d.record)) {
		t.Run(name, func(t *testing.T) {
			p, c := newManagedPanel(t, reviewForeignCredential(t, alias))
			if _, err := c.CreateDisabled(context.Background(), d); !errors.Is(err, ErrManagedConflict) || p.countPosts() != 0 {
				t.Fatal("equivalent protocol UUID permitted create")
			}
			if _, found, err := c.FindCreated(context.Background(), d); found || !errors.Is(err, ErrManagedConflict) || p.countPosts() != 0 {
				t.Fatal("equivalent protocol UUID permitted absence/recovery")
			}
		})
	}
}

func TestManagedEnableRechecksCrossAccountCredentials(t *testing.T) {
	r := managedTestRecord(t, 123)
	d, err := PrepareEnable(r, Binding{r.ID(), r.Name(), r.CredentialsHash()})
	if err != nil {
		t.Fatal("prepare failed")
	}
	for name, alias := range reviewAliases(reviewUUID(t, r)) {
		t.Run(name, func(t *testing.T) {
			p, c := newManagedPanel(t, r, reviewForeignCredential(t, alias))
			if _, err := c.Enable(context.Background(), d); !errors.Is(err, ErrManagedConflict) || p.countPosts() != 0 {
				t.Fatal("equivalent credentials in another account permitted enable")
			}
		})
	}
	t.Run("readback", func(t *testing.T) {
		p, c := newManagedPanel(t, r)
		p.afterSave = func(p *managedPanel, _ ClientRecord, _ string) {
			p.records[124] = reviewForeignCredential(t, reviewUUID(t, r))
		}
		if _, err := c.Enable(context.Background(), d); !errors.Is(err, ErrManagedUnknown) || p.countPosts() != 1 {
			t.Fatal("post-write credential conflict was treated as success")
		}
	})
}

func TestManagedQuotaZeroCountersRefuseResetRisk(t *testing.T) {
	r := managedTestRecord(t, 123)
	r.wire.Volume = 15
	binding := Binding{r.ID(), r.Name(), r.CredentialsHash()}
	if _, err := PrepareEnable(r, binding); !errors.Is(err, ErrManagedState) {
		t.Fatal("quota with zero counters prepared as an edit")
	}
	// Counters can change while an intent waits. Even a previously safe image
	// must be refused when the fresh save would carry an implicit reset.
	r.wire.Up = 1
	d, err := PrepareEnable(r, binding)
	if err != nil {
		t.Fatal("non-reset intent rejected")
	}
	r.wire.Up = 0
	p, c := newManagedPanel(t, r)
	if _, err := c.Enable(context.Background(), d); !errors.Is(err, ErrManagedState) || p.countPosts() != 0 {
		t.Fatal("fresh zero counters could reset concurrent quota usage")
	}
}
