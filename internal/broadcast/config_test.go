package broadcast

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

func TestBroadcastDefaultOffIgnoresUnusedPrivateSettings(t *testing.T) {
	t.Setenv("BROADCAST_ENABLED", "false")
	t.Setenv("BROADCAST_POLICY_REVISION", "unused")
	t.Setenv("BROADCAST_WORKERS", "unused")
	cfg, err := ConfigFromEnv(false, nil)
	if err != nil || cfg.Enabled {
		t.Fatal("disabled broadcast requires unused integration settings")
	}
}
func TestBroadcastPolicyCanonicalScopeAndBounds(t *testing.T) {
	cfg := Config{Enabled: true, Revision: 1, MaxRecipients: 100, BatchSize: 50, Workers: 1, PollInterval: time.Second, AdminIDs: map[int64]struct{}{8: {}, 7: {}}}
	p := cfg.Policy()
	if cfg.Validate(true) != nil || p.Validate() != nil || p.AdminIDs[0] != 7 || p.AdminIDs[1] != 8 {
		t.Fatal("canonical policy rejected")
	}
	cfg.AdminIDs[9] = struct{}{}
	if len(p.AdminIDs) != 2 || p.Validate() != nil {
		t.Fatal("policy aliases mutable administrator configuration")
	}
	q := p
	q.AdminIDs = []int64{7, 9}
	if q.Validate() == nil {
		t.Fatal("policy hash accepted different administrators")
	}
	q = p
	q.MaxRecipients++
	if q.Validate() == nil {
		t.Fatal("policy hash accepted a different audience cap")
	}
	q = p
	q.Revision++
	if q.Validate() != nil || q.Equal(p) {
		t.Fatal("explicit revision is not an independent fence")
	}
	cfg.AdminIDs = map[int64]struct{}{}
	for i := int64(1); i <= 1001; i++ {
		cfg.AdminIDs[i] = struct{}{}
	}
	if cfg.Validate(true) == nil {
		t.Fatal("unbounded administrator scope accepted")
	}
}
func TestBroadcastBodyAndCommandDoNotBecomeProtectedIntentOrLogs(t *testing.T) {
	for _, body := range []string{"", " \t", queue.ConfigurationIntentBody, string([]byte{0xff}), "\x00"} {
		if ValidateBody(body) == nil {
			t.Fatal("unsafe ordinary broadcast body accepted")
		}
	}
	private := "SYNTHETIC_PRIVATE_BROADCAST_CONTENT"
	cmd := Command{Name: "/broadcast_text", Argument: private}
	b, err := json.Marshal(cmd)
	if err != nil || strings.Contains(string(b), private) || strings.Contains(fmt.Sprintf("%+v %#v", cmd, cmd), private) {
		t.Fatal("private command disclosed through ordinary serialization")
	}
	u := telegram.Update{ID: 1, Message: &telegram.Message{From: &telegram.User{ID: 7}, Chat: telegram.Chat{ID: 7, Type: "private"}, Text: "/broadcast_text\u2003" + private, Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: 15}}}}
	_, parsed, ok := ParseCommand(u, "mybot", map[int64]struct{}{7: {}})
	if !ok || parsed.Argument != private || ValidateBody(parsed.Argument) != nil {
		t.Fatal("UTF-8 delimiter corrupted a private message")
	}
}
