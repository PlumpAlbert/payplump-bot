package invoiceninja

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestImportedAlreadyPausedConfirmationDoesNotWrite(t *testing.T) {
	_, before := importedFixture(t, "3")
	before.NextSendDate, before.NextSendDatetime = "", ""
	profile, err := NewImportedRecurringProfile(before, before.LineItems[0].ProductKey)
	if err != nil || !profile.BaselinePaused() {
		t.Fatalf("frozen pause rejected: %v", err)
	}
	draft, err := PrepareImportedPause(profile, before)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var restored ImportedPauseDraft
	if err := json.Unmarshal(raw, &restored); err != nil || restored.ProfileHash() != profile.Hash() {
		t.Fatalf("already-paused observation lost journal binding: %v", err)
	}
	gets, authorizations := 0, 0
	server := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("already-paused observation issued a provider mutation")
		}
		gets++
		recurringTestReply(t, w, map[string]any{"data": before})
	})
	client, err := NewImportedRecurringClient(server.URL, recurringTestToken, time.Second, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.PauseAuthorized(context.Background(), restored, func(context.Context) error {
		authorizations++
		return nil
	})
	if err != nil || gets != 2 || authorizations != 1 || !lifecycleSameImportedFields(got, before) {
		t.Fatalf("already-paused confirmation changed source or skipped authorization: gets=%d auth=%d err=%v", gets, authorizations, err)
	}
	activeProfile, active := importedFixture(t, "2")
	active.StatusID = "3"
	if _, err := PrepareImportedPause(activeProfile, active); err == nil {
		t.Fatal("unobserved manual pause accepted under an originally active profile")
	}
	changed := before
	changed.NextSendDate = "2040-02-29"
	if ValidateImportedRecurringBaseline(changed, profile) == nil || ValidateImportedRecurringBaseline(before, profile) != nil {
		t.Fatal("paused baseline stopped binding its observed schedule")
	}
}
