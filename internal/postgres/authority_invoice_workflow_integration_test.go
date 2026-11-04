//go:build integration

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
	"github.com/PlumpAlbert/payplump-bot/internal/outbox"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type invoiceWorkflowSource struct{}

func (invoiceWorkflowSource) ListCustomers(context.Context) ([]invoiceninja.Customer, error) {
	return []invoiceninja.Customer{{ID: "invoice-customer", IDNumber: "42"}}, nil
}
func (invoiceWorkflowSource) ListRecurring(context.Context, string) ([]invoiceninja.RecurringRecord, error) {
	return []invoiceninja.RecurringRecord{{ID: "invoice-recurring", ClientID: "invoice-customer", StatusID: "2", NextSendDate: "2040-03-01"}}, nil
}

type invoiceWorkflowPanel struct{ record sui.ClientRecord }

func (p invoiceWorkflowPanel) Inventory(context.Context) ([]sui.ClientRecord, error) {
	return []sui.ClientRecord{p.record}, nil
}
func (invoiceWorkflowPanel) FindCreated(context.Context, sui.AuthorityCreateDraft) (sui.ClientRecord, bool, error) {
	return sui.ClientRecord{}, false, errors.New("unexpected panel lookup")
}
func (invoiceWorkflowPanel) CreateAuthorized(context.Context, sui.AuthorityCreateDraft, func(context.Context) error) (sui.ClientRecord, error) {
	return sui.ClientRecord{}, errors.New("unexpected panel write")
}

type invoiceWorkflowHTTP struct {
	ninjaHost string
	local     http.RoundTripper
	mu        sync.Mutex
	attempts  map[int64][]string
}

func (g *invoiceWorkflowHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == g.ninjaHost && r.URL.Path == "/api/v1/invoices" && r.Method == http.MethodGet {
		return g.local.RoundTrip(r)
	}
	if r.URL.Scheme != "https" || r.URL.Host != "api.telegram.org" || r.URL.Path != "/botSYNTHETIC/sendMessage" || r.Method != http.MethodPost {
		return nil, errors.New("unexpected non-synthetic HTTP request")
	}
	var body struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.ChatID != 42 && body.ChatID != 99 && body.ChatID != 100 && body.ChatID != 101 {
		return nil, errors.New("unexpected invoice notification recipient")
	}
	g.mu.Lock()
	g.attempts[body.ChatID] = append(g.attempts[body.ChatID], body.Text)
	g.mu.Unlock()
	if body.ChatID == 101 {
		// One blocked admin must not prevent the client or other admins receiving notices.
		return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":403,"description":"Forbidden"}`)), Request: r}, nil
	}
	bodyJSON := fmt.Sprintf(`{"ok":true,"result":{"message_id":1,"chat":{"id":%d,"type":"private"}}}`, body.ChatID)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(bodyJSON)), Request: r}, nil
}

// Real invoice HTTP decoding, authenticated webhook, durable wake, authority
// reconciliation, ledger/outbox and Telegram sender run together. All HTTP is
// confined to synthetic loopback/transport; PostgreSQL runs only in disposable CI.
func TestAuthorityInvoiceWebhookAndPollingDeliverEveryNewInvoice(t *testing.T) {
	s, ctx := authorityFixture(t)
	const bot = int64(9123)
	var mu sync.Mutex
	makeInvoice := func(id, number, status, date, recurring string) invoiceninja.CycleInvoice {
		return invoiceninja.CycleInvoice{ID: id, ClientID: "invoice-customer", RecurringID: recurring, Number: number, StatusID: status, Date: date, DueDate: date, Amount: "100", Balance: "100", PaidToDate: "0", ExchangeRate: "1", Invitations: []invoiceninja.CycleInvitation{}}
	}
	invoices := []invoiceninja.CycleInvoice{makeInvoice("history", "HISTORY", "4", "2040-02-01", "invoice-recurring")}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/invoices" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		rows := append([]invoiceninja.CycleInvoice{}, invoices...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows, "meta": map[string]any{"pagination": map[string]int{"total": len(rows), "count": len(rows), "per_page": 50, "current_page": 1, "total_pages": 1}}})
	}))
	defer api.Close()
	apiURL, _ := url.Parse(api.URL)
	gate := &invoiceWorkflowHTTP{ninjaHost: apiURL.Host, local: api.Client().Transport, attempts: make(map[int64][]string)}
	old := http.DefaultTransport
	http.DefaultTransport = gate
	defer func() { http.DefaultTransport = old }()
	reader, err := invoiceninja.NewCycleReader(api.URL, "SYNTHETIC", time.Second, 50, 20)
	if err != nil {
		t.Fatal(err)
	}
	panel := invoiceWorkflowPanel{record: authorityRecord(t, authorityDraft(t, "42", true), 123)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := ninjaauthority.Config{BotID: bot, SourceScopeHash: strings.Repeat("a", 64), InboundIDs: []int64{1, 2}, AdminIDs: []int64{99, 100, 101, 99}}
	service, err := ninjaauthority.New(s, invoiceWorkflowSource{}, reader, panel, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM outbox)`)

	hook, err := lifecycle.NewWebhook("SYNTHETIC_INVOICE_SECRET", s)
	if err != nil {
		t.Fatal(err)
	}
	if err = hook.Activate(bot); err != nil {
		t.Fatal(err)
	}
	post := func(secret string, want int) {
		t.Helper()
		// The foreign ID is deliberately ignored: only canonical API inventory counts.
		request := httptest.NewRequest(http.MethodPost, lifecycle.WebhookPath, bytes.NewBufferString(`{"id":"untrusted-foreign-invoice"}`)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(lifecycle.WebhookSecretHeader, secret)
		response := httptest.NewRecorder()
		hook.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("webhook status=%d want=%d", response.Code, want)
		}
	}
	worker, err := outbox.New(s, telegram.NewClient("SYNTHETIC"), bot, outbox.Config{Workers: 1, PollInterval: 100 * time.Millisecond}, logger)
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorkers := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	defer func() { stopWorkers(); <-workerDone }()
	scanCtx, stopScan := context.WithCancel(ctx)
	scanDone := make(chan error, 1)
	go func() { scanDone <- service.Run(scanCtx) }()
	scanStopped := false
	defer func() {
		if !scanStopped {
			stopScan()
			<-scanDone
		}
	}()
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			if check() {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-deadline.C:
				t.Fatal("synthetic authority workflow did not progress")
			case <-ticker.C:
			}
		}
	}
	wait(func() bool {
		var revision int64
		err := s.pool.QueryRow(ctx, `SELECT revision FROM authority_observations WHERE bot_id=$1 AND actor_id=42`, bot).Scan(&revision)
		return err == nil && revision >= 2
	})
	countSent := func(want int) bool {
		var complete bool
		err := s.pool.QueryRow(ctx, `SELECT
 count(*) FILTER(WHERE chat_id IN (42,99,100) AND status='sent')=$2*3
 AND count(*) FILTER(WHERE chat_id=101 AND status='failed' AND last_error_code=403)=$2
 AND count(*)=$2*4 FROM outbox WHERE bot_id=$1`, bot, want).Scan(&complete)
		return err == nil && complete
	}
	// Same day, unrelated recurring ID and draft status must not suppress creation.
	mu.Lock()
	invoices = append(invoices, makeInvoice("manual", "MANUAL", "1", "2040-02-01", ""), makeInvoice("another", "ANOTHER", "2", "2040-02-01", "other-recurring"))
	mu.Unlock()
	post("wrong-secret", http.StatusForbidden)
	generation, err := s.LoadAuthorityWake(ctx, bot)
	if err != nil || generation.Invoices != 0 {
		t.Fatalf("rejected hook changed wake: %+v %v", generation, err)
	}
	post("SYNTHETIC_INVOICE_SECRET", http.StatusOK)
	wait(func() bool { return countSent(2) }) // This must happen on the wake, not the five-minute timer.
	mu.Lock()
	invoices[1].StatusID = "2"
	mu.Unlock()
	post("SYNTHETIC_INVOICE_SECRET", http.StatusOK)
	wait(func() bool { return countSent(3) })
	// An identical webhook replay can wake another scan, but cannot duplicate notices.
	for range 3 {
		post("SYNTHETIC_INVOICE_SECRET", http.StatusOK)
	}
	stopScan()
	if err = <-scanDone; err != nil {
		t.Fatal(err)
	}
	scanStopped = true
	restarted, err := ninjaauthority.New(s, invoiceWorkflowSource{}, reader, panel, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=3 FROM outbox WHERE bot_id=$1 AND chat_id=42`, bot)
	// A backdated invoice discovered by polling, without any webhook, is still new.
	mu.Lock()
	invoices = append(invoices, makeInvoice("backdated", "BACKDATED", "2", "2039-01-01", ""))
	mu.Unlock()
	if err = restarted.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return countSent(4) })
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM payment_confirmations) AND NOT EXISTS(SELECT 1 FROM subscriptions WHERE payment_date IS NOT NULL) AND NOT EXISTS(SELECT 1 FROM outbox WHERE protected_configuration OR client_menu)`)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	for _, recipient := range []int64{42, 99, 100, 101} {
		texts := gate.attempts[recipient]
		counts := map[string]int{}
		for _, body := range texts {
			if strings.Contains(body, "https://") || strings.Contains(body, "SYNTHETIC") {
				t.Fatal("ordinary invoice notice contains synthetic URL/credential")
			}
			for _, number := range []string{"HISTORY", "MANUAL", "ANOTHER", "BACKDATED"} {
				if strings.Contains(body, number) {
					counts[number]++
				}
			}
		}
		if len(texts) != 4 || counts["HISTORY"] != 0 || counts["MANUAL"] != 2 || counts["ANOTHER"] != 1 || counts["BACKDATED"] != 1 {
			t.Fatalf("unexpected synthetic attempts for %d: %+v", recipient, counts)
		}
	}
}
