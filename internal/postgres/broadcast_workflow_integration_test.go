//go:build integration

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/outbox"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type broadcastHTTPGate struct {
	loop          *httptest.Server
	client        *http.Client
	mu            sync.Mutex
	sent          map[int64]int
	broadcastSent map[int64]int
	texts         []string
}

func (g *broadcastHTTPGate) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != "api.telegram.org" || r.URL.Path != "/botSYNTHETIC/sendMessage" || r.Method != http.MethodPost {
		return nil, context.Canceled
	}
	var msg struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(body, &msg) != nil {
		return nil, context.Canceled
	}
	g.mu.Lock()
	g.sent[msg.ChatID]++
	if msg.Text == "Synthetic broadcast body" {
		g.broadcastSent[msg.ChatID]++
	}
	g.texts = append(g.texts, msg.Text)
	g.mu.Unlock()
	if msg.ChatID == 8 {
		return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"blocked"}`)), Request: r}, nil
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, g.loop.URL+"/send", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return g.client.Do(req)
}

type noBroadcastFallback struct{}

func (noBroadcastFallback) ApplyTelegramUpdate(context.Context, int64, string, telegram.Update) error {
	return context.Canceled
}
func broadcastResponseToken(t *testing.T, body, separator string) string {
	t.Helper()
	_, tail, ok := strings.Cut(body, separator)
	fields := strings.Fields(tail)
	if !ok || len(fields) == 0 {
		t.Fatal("expected broadcast confirmation token missing")
	}
	if _, valid := tokenHash(fields[0]); !valid {
		t.Fatal("invalid broadcast confirmation token")
	}
	return fields[0]
}
func TestBroadcastWorkflowWithSyntheticHTTP(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	const bot = int64(876543)
	cfg := broadcast.Config{Enabled: true, Revision: 1, MaxRecipients: 1000, BatchSize: 2, Workers: 2, PollInterval: 100 * time.Millisecond, AdminIDs: map[int64]struct{}{7: {}}}
	policy := cfg.Policy()
	if err := s.InstallBroadcastPolicy(ctx, bot, policy); err != nil {
		t.Fatal("install policy")
	}
	for _, id := range []int64{8, 9} {
		r, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: id, ChatID: id, DisplayName: "synthetic"})
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = recordBroadcastMembership(ctx, tx, bot, id); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		_ = r
	}
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send" {
			http.NotFound(w, r)
			return
		}
		var q struct {
			ChatID int64 `json:"chat_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 1, "chat": map[string]any{"id": q.ChatID, "type": "private"}}})
	}))
	defer loop.Close()
	gate := &broadcastHTTPGate{loop: loop, client: loop.Client(), sent: map[int64]int{}, broadcastSent: map[int64]int{}}
	old := http.DefaultTransport
	http.DefaultTransport = gate
	defer func() { http.DefaultTransport = old }()
	client := telegram.NewClient("SYNTHETIC")
	const webhookSecret = "SYNTHETIC_BROADCAST_WEBHOOK_SECRET"
	endpoint, err := telegram.NewWebhookHandler(webhookSecret, broadcast.NewGateway(noBroadcastFallback{}, s, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err = endpoint.Activate(telegram.User{ID: bot, IsBot: true, Username: "mybot"}); err != nil {
		t.Fatal(err)
	}
	command := func(n int64, text string) {
		t.Helper()
		head := text
		if i := strings.IndexAny(text, " \n\t"); i >= 0 {
			head = text[:i]
		}
		u := broadcastUpdate(n, text)
		u.Message.Entities = []telegram.Entity{{Type: "bot_command", Offset: 0, Length: len(head)}}
		raw, err := json.Marshal(u)
		if err != nil {
			t.Fatal("encode synthetic update")
		}
		req := httptest.NewRequest(http.MethodPost, "http://loopback.invalid/telegram/webhook", bytes.NewReader(raw)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(telegram.WebhookSecretHeader, webhookSecret)
		response := httptest.NewRecorder()
		endpoint.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("broadcast webhook command failed: HTTP %d", response.Code)
		}
	}
	command(1, "/broadcast")
	command(2, "/broadcast_text Synthetic broadcast body")
	var preview string
	if err := s.pool.QueryRow(ctx, `SELECT body FROM outbox WHERE message_key=$1`, `broadcast-admin:`+formatIDs(bot, 2)).Scan(&preview); err != nil {
		t.Fatal("preview missing")
	}
	tokenA := broadcastResponseToken(t, preview, "Токен: ")
	command(3, "/broadcast_all "+tokenA)
	var freeze string
	if err := s.pool.QueryRow(ctx, `SELECT body FROM outbox WHERE message_key=$1`, `broadcast-admin:`+formatIDs(bot, 3)).Scan(&freeze); err != nil {
		t.Fatal(err)
	}
	tokenB := broadcastResponseToken(t, freeze, "/broadcast_confirm ")
	// This recipient becomes genuinely eligible AFTER preview but BEFORE confirm.
	lateUpdate := telegram.Update{ID: 1000, Message: &telegram.Message{From: &telegram.User{ID: 10, FirstName: "late"}, Chat: telegram.Chat{ID: 10, Type: "private"}, Text: "/start", Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: 6}}}}
	if err := s.ApplyMenuUpdate(ctx, bot, "mybot", lateUpdate, clientmenu.Prepared{Request: clientmenu.Request{BotID: bot, UpdateID: 1000, TelegramID: 10, ChatID: 10, Action: clientmenu.Start}, Text: "synthetic hello"}); err != nil {
		t.Fatal("late private menu interaction failed")
	}
	command(4, "/broadcast_confirm "+tokenB)
	command(4, "/broadcast_confirm "+tokenB) // exact raw update replay is idempotent
	command(5, "/broadcast_confirm "+tokenB) // new update ID also creates no second job
	command(6, "/broadcast_status "+tokenB)
	// A separate draft exercises cancel; it must not alter the confirmed job.
	command(7, "/broadcast_text Cancel this draft")
	var preview2 string
	if err := s.pool.QueryRow(ctx, `SELECT body FROM outbox WHERE message_key=$1`, `broadcast-admin:`+formatIDs(bot, 7)).Scan(&preview2); err != nil {
		t.Fatal(err)
	}
	tokenC := broadcastResponseToken(t, preview2, "Токен: ")
	command(8, "/broadcast_cancel "+tokenC)
	var logBuffer bytes.Buffer
	bw, err := broadcast.New(s, bot, cfg, slog.New(slog.NewJSONHandler(&logBuffer, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ow, err := outbox.New(s, client, bot, outbox.Config{Workers: 2, PollInterval: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = bw.Run(workerCtx) }()
	go func() { defer wg.Done(); _ = ow.Run(workerCtx) }()
	defer func() { stop(); wg.Wait() }()
	deadline := time.NewTimer(30 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	complete := false
	for !complete {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-deadline.C:
			t.Fatal("broadcast/outbox did not complete")
		case <-ticker.C:
			var state string
			err = s.pool.QueryRow(ctx, `SELECT state FROM broadcasts WHERE bot_id=$1`, bot).Scan(&state)
			if err == nil && state == "done" {
				var summary string
				err = s.pool.QueryRow(ctx, `SELECT status FROM outbox WHERE message_key=(SELECT 'broadcast-summary:'||id FROM broadcasts WHERE bot_id=$1)`, bot).Scan(&summary)
				complete = err == nil && summary == "sent"
			}
		}
	}
	stop()
	wg.Wait()
	// A fresh durable batch worker after completion must not recreate any job or send.
	restarted, err := broadcast.New(s, bot, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	restartCtx, restartCancel := context.WithTimeout(ctx, 250*time.Millisecond)
	if err = restarted.Run(restartCtx); err != nil {
		restartCancel()
		t.Fatal(err)
	}
	restartCancel()
	var jobs int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcasts WHERE bot_id=$1`, bot).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("jobs=%d err=%v", jobs, err)
	}
	var late int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_deliveries d JOIN clients c ON c.id=d.client_id JOIN broadcasts b ON b.id=d.job_id WHERE b.bot_id=$1 AND c.telegram_id=10`, bot).Scan(&late); err != nil || late != 0 {
		t.Fatalf("late recipient included: %d err=%v", late, err)
	}
	gate.mu.Lock()
	broadcastRecipients := map[int64]int{}
	for k, v := range gate.broadcastSent {
		broadcastRecipients[k] = v
	}
	gate.mu.Unlock()
	if len(broadcastRecipients) != 2 || broadcastRecipients[8] != 1 || broadcastRecipients[9] != 1 {
		t.Fatalf("unexpected broadcast deliveries: %#v", broadcastRecipients)
	}
	if strings.Contains(logBuffer.String(), tokenA) || strings.Contains(logBuffer.String(), tokenB) || strings.Contains(logBuffer.String(), "Synthetic broadcast body") {
		t.Fatal("broadcast content or token leaked into logs")
	}
	var outcomes []struct{ Status string }
	rows, err := s.pool.Query(ctx, `SELECT o.status FROM broadcast_deliveries d JOIN broadcasts b ON b.id=d.job_id JOIN outbox o ON o.id=d.outbox_id WHERE b.bot_id=$1 ORDER BY d.chat_id`, bot)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var outcome struct{ Status string }
		if err = rows.Scan(&outcome.Status); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		outcomes = append(outcomes, outcome)
	}
	rows.Close()
	if len(outcomes) != 2 {
		t.Fatalf("unexpected delivery rows: %d", len(outcomes))
	}
	var sentCount, failedCount int
	for _, o := range outcomes {
		if o.Status == "sent" {
			sentCount++
		}
		if o.Status == "failed" {
			failedCount++
		}
	}
	if sentCount != 1 || failedCount != 1 {
		t.Fatalf("unexpected durable outcomes: %+v", outcomes)
	}
	var summaries int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE bot_id=$1 AND message_key LIKE 'broadcast-summary:%'`, bot).Scan(&summaries); err != nil || summaries != 1 {
		t.Fatalf("summary rows=%d err=%v", summaries, err)
	}
}
func formatIDs(bot, update int64) string {
	return strconv.FormatInt(bot, 10) + ":" + strconv.FormatInt(update, 10)
}
