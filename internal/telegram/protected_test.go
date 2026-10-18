package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type protectedTransport func(*http.Request) (*http.Response, error)

func (f protectedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func preparedText(ctx context.Context) (ProtectedMessage, error) {
	return ProtectedMessage{Text: "sensitive-configuration", NotAfter: time.Now().Add(time.Minute)}, nil
}

func TestGetPrivateChatExactScope(t *testing.T) {
	for _, result := range []string{
		`{"id":42,"type":"private","first_name":"private-user","username":"sensitive-user"}`,
		`{"id":99,"type":"private"}`, `{"id":-42,"type":"private"}`,
		`{"id":42,"type":"group"}`, `{"id":42,"type":"supergroup"}`, `{"id":42,"type":"channel"}`,
		`{"id":42}`, `{"type":"private"}`, `{"id":42,"type":null}`,
		`{"id":99,"id":42,"type":"private"}`,
	} {
		var calls atomic.Int32
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			body, _ := io.ReadAll(r.Body)
			if r.Method != "POST" || r.URL.Path != "/bottest-only-token/getChat" || string(body) != `{"chat_id":42}` {
				t.Error("getChat exposed extra data or incorrect scope")
			}
			fmt.Fprintf(w, `{"ok":true,"result":%s}`, result)
		})
		err := c.GetPrivateChat(context.Background(), 42)
		if (err == nil) != strings.Contains(result, "private-user") || calls.Load() != 1 {
			t.Fatal("incorrect private chat verification")
		}
		if strings.Contains(fmt.Sprint(err), "private-user") || strings.Contains(fmt.Sprint(err), "sensitive-user") {
			t.Fatal("chat identity leaked")
		}
		for _, id := range []int64{0, -42} {
			if err := c.GetPrivateChat(context.Background(), id); err == nil {
				t.Fatal("invalid chat ID accepted")
			}
		}
		if calls.Load() != 1 {
			t.Fatal("invalid chat ID caused I/O")
		}
	}
}

func TestSendProtectedMessageFlagsAndPrepareOrder(t *testing.T) {
	const chatID int64 = 4503599627370497
	text := strings.Repeat("я", 4096)
	var prepared, posts atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if prepared.Load() != 1 || r.Method != "POST" || r.URL.Path != "/bottest-only-token/sendMessage" {
			t.Error("POST preceded authorization or wrong request")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid JSON")
			return
		}
		var id int64
		var gotText string
		if len(body) != 5 || json.Unmarshal(body["chat_id"], &id) != nil || id != chatID || json.Unmarshal(body["text"], &gotText) != nil || gotText != text {
			t.Error("protected payload changed or added fields")
		}
		if string(body["protect_content"]) != "true" || string(body["disable_notification"]) != "true" || string(body["link_preview_options"]) != `{"is_disabled":true}` {
			t.Error("missing protection/preview/notification restrictions")
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":7,"chat":{"id":%d,"type":"private"}}}`, chatID)
	})
	id, err := c.SendProtectedMessage(context.Background(), chatID, func(ctx context.Context) (ProtectedMessage, error) {
		prepared.Add(1)
		if posts.Load() != 0 {
			t.Fatal("network before prepare")
		}
		return ProtectedMessage{Text: text, NotAfter: time.Now().Add(time.Minute)}, nil
	})
	if err != nil || id != 7 || prepared.Load() != 1 || posts.Load() != 1 {
		t.Fatal("incorrect protected send", err)
	}
}

func TestSendProtectedMessagePreflightAndCallbackRejections(t *testing.T) {
	var posts atomic.Int32
	c := testClient(t, func(http.ResponseWriter, *http.Request) { posts.Add(1) })
	var calls int
	prepare := func(context.Context) (ProtectedMessage, error) { calls++; return preparedText(context.Background()) }
	for _, id := range []int64{0, -1} {
		if _, err := c.SendProtectedMessage(context.Background(), id, prepare); err == nil {
			t.Fatal("invalid chat accepted")
		}
	}
	if _, err := c.SendProtectedMessage(context.Background(), 42, nil); err == nil {
		t.Fatal("nil callback accepted")
	}
	if calls != 0 || posts.Load() != 0 {
		t.Fatal("invalid arguments invoked callback or network")
	}
	denied := errors.New("authorization denied")
	_, err := c.SendProtectedMessage(context.Background(), 42, func(context.Context) (ProtectedMessage, error) { calls++; return ProtectedMessage{}, denied })
	if !errors.Is(err, denied) || calls != 1 || posts.Load() != 0 {
		t.Fatal("callback failure lost or attempted POST")
	}
	for _, mode := range []string{"canceled", "expired", "parent-expired"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var called int
			_, err := c.SendProtectedMessage(ctx, 42, func(callbackCtx context.Context) (ProtectedMessage, error) {
				called++
				if callbackCtx != ctx {
					t.Fatal("prepare received a different authorization context")
				}
				message, _ := preparedText(ctx)
				switch mode {
				case "canceled":
					cancel()
				case "expired":
					message.NotAfter = time.Now().Add(-time.Second)
				case "parent-expired":
					cancel()
					message.NotAfter = time.Now().Add(-time.Second)
				}
				return message, nil
			})
			if err == nil || called != 1 || posts.Load() != 0 {
				t.Fatal("post-callback cancellation/expiry attempted I/O")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := calls
	if _, err := c.SendProtectedMessage(ctx, 42, prepare); !errors.Is(err, context.Canceled) || calls != before || posts.Load() != 0 {
		t.Fatal("already canceled request proceeded")
	}
}

func TestSendProtectedMessageValidationAndFormatting(t *testing.T) {
	for _, message := range []ProtectedMessage{
		{Text: "", NotAfter: time.Now().Add(time.Minute)},
		{Text: string([]byte{0xff}), NotAfter: time.Now().Add(time.Minute)},
		{Text: "sensitive\x00configuration", NotAfter: time.Now().Add(time.Minute)},
		{Text: strings.Repeat("я", 4097), NotAfter: time.Now().Add(time.Minute)},
		{Text: "sensitive-configuration"},
		{Text: "sensitive-configuration", NotAfter: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		c := testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid protected message sent") })
		calls := 0
		_, err := c.SendProtectedMessage(context.Background(), 42, func(context.Context) (ProtectedMessage, error) { calls++; return message, nil })
		if err == nil || calls != 1 || strings.Contains(fmt.Sprint(err), "sensitive") {
			t.Fatal("invalid protected text/deadline accepted or disclosed")
		}
	}
	message, _ := preparedText(context.Background())
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, value := range []any{message, &message} {
			if got := fmt.Sprintf(format, value); strings.Contains(got, message.Text) || !strings.Contains(got, "sensitive Telegram message") {
				t.Fatal("message formatting not redacted")
			}
		}
	}
}

func TestSendProtectedMessageDeadlineAndNoReplayBody(t *testing.T) {
	for _, parentEarlier := range []bool{false, true} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":42,"type":"private"}}}`)
		})
		now := time.Now()
		notAfter, parentDeadline := now.Add(5*time.Second), now.Add(10*time.Second)
		if parentEarlier {
			parentDeadline = now.Add(3 * time.Second)
		}
		expected := notAfter
		if parentEarlier {
			expected = parentDeadline
		}
		ctx, cancel := context.WithDeadline(context.Background(), parentDeadline)
		var seen bool
		c.http.Transport = protectedTransport(func(r *http.Request) (*http.Response, error) {
			seen = true
			deadline, ok := r.Context().Deadline()
			if !ok || !deadline.Equal(expected) || r.GetBody != nil {
				t.Error("incorrect child deadline or replayable write")
			}
			return http.DefaultTransport.RoundTrip(r)
		})
		_, err := c.SendProtectedMessage(ctx, 42, func(context.Context) (ProtectedMessage, error) {
			return ProtectedMessage{Text: "secret", NotAfter: notAfter}, nil
		})
		cancel()
		if err != nil || !seen {
			t.Fatal("send failed", err)
		}
	}
}

func TestSendProtectedMessageDeadlineCancelsAttempt(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer close(release)
	_, err := c.SendProtectedMessage(context.Background(), 42, func(context.Context) (ProtectedMessage, error) {
		return ProtectedMessage{Text: "secret", NotAfter: time.Now().Add(100 * time.Millisecond)}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatal("deadline failed to cancel a single attempt", err)
	}
}

func TestSendProtectedMessageStrictReceipt(t *testing.T) {
	for _, body := range []string{
		`{"ok":true,"result":{}}`,
		`{"ok":true,"result":{"message_id":0,"chat":{"id":42,"type":"private"}}}`,
		`{"ok":true,"result":{"message_id":7,"chat":{"id":99,"type":"private"}}}`,
		`{"ok":true,"result":{"message_id":7,"chat":{"id":42,"type":"group"}}}`,
		`{"ok":true,"result":{"message_id":7,"chat":{"id":42}}}`,
		`{"ok":true,"result":{"message_id":7,"chat":{"id":99,"id":42,"type":"private"}}}`,
		`{"ok":false,"ok":true,"result":{"message_id":7,"chat":{"id":42,"type":"private"}}}`,
	} {
		var calls atomic.Int32
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, body) })
		if id, err := c.SendProtectedMessage(context.Background(), 42, preparedText); err == nil || id != 0 || calls.Load() != 1 {
			t.Fatal("invalid receipt accepted or retried")
		}
	}
}

func TestProtectedDeliveryKnownVersusUnknownRejection(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		api, rejected bool
	}{
		{"explicit-200", 200, `{"ok":false,"error_code":403}`, true, true},
		{"explicit-400", 400, `{"ok":false,"error_code":400}`, true, true},
		{"forbidden", 403, `{"ok":false,"error_code":403,"description":"sensitive-configuration test-only-token"}`, true, true},
		{"rate-limit", 429, `{"ok":false,"error_code":429,"parameters":{"retry_after":17}}`, true, true},
		{"rate-limit-200", 200, `{"ok":false,"error_code":429}`, true, true},
		{"timeout-code", 200, `{"ok":false,"error_code":408}`, true, false},
		{"timeout-http", 408, `{"ok":false,"error_code":400}`, true, false},
		{"server-error", 500, `{"ok":false,"error_code":403}`, true, false},
		{"server-code", 400, `{"ok":false,"error_code":500}`, true, false},
		{"ok-with-error-http", 403, `{"ok":true,"error_code":403}`, true, false},
		{"malformed-http", 403, `sensitive-configuration test-only-token`, true, false},
		{"missing-ok", 400, `{"error_code":400}`, true, false},
		{"missing-code", 400, `{"ok":false}`, true, false},
		{"null-code", 400, `{"ok":false,"error_code":null}`, true, false},
		{"wrong-code-type", 400, `{"ok":false,"error_code":"400"}`, true, false},
		{"duplicate-ok", 400, `{"ok":true,"ok":false,"error_code":400}`, true, false},
		{"case-duplicate", 400, `{"OK":true,"ok":false,"error_code":400}`, true, false},
		{"duplicate-code", 400, `{"ok":false,"error_code":500,"error_code":400}`, true, false},
		{"bad-parameters", 400, `{"ok":false,"error_code":400,"parameters":true}`, true, false},
		{"duplicate-nested", 400, `{"ok":false,"error_code":400,"parameters":{"retry_after":1,"retry_after":2}}`, true, false},
		{"trailing-json", 400, `{"ok":false,"error_code":400}{}`, true, false},
		{"not-200", 201, `{"ok":false,"error_code":400}`, true, false},
		{"redirect", 307, `{"ok":false,"error_code":403}`, true, false},
		{"malformed-success-status", 200, `bad`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			_, err := c.SendProtectedMessage(context.Background(), 42, preparedText)
			var apiErr *APIError
			if err == nil || errors.As(err, &apiErr) != tc.api || (apiErr != nil && apiErr.Rejected != tc.rejected) || calls.Load() != 1 {
				t.Fatal("incorrect known-rejection classification", err)
			}
			for _, formatted := range []string{fmt.Sprint(err), fmt.Sprintf("%#v", err)} {
				if strings.Contains(formatted, "sensitive-configuration") || strings.Contains(formatted, "test-only-token") || strings.Contains(formatted, c.baseURL) {
					t.Fatal("sensitive error retained")
				}
			}
			if tc.name == "rate-limit" && (apiErr.RetryAfter != 17*time.Second || apiErr.Permanent()) {
				t.Fatal("cooldown contract changed")
			}
			// Ordinary callers retain existing permanent-error policy even when
			// a malformed response provides no evidence of rejected delivery.
			if tc.name == "malformed-http" && (!apiErr.Permanent() || apiErr.Error() != "Telegram API error: HTTP 403, code 403") {
				t.Fatal("ordinary error compatibility changed")
			}
		})
	}
}

func TestSendProtectedMessageNoRedirectOrAutomaticRetry(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	for _, mode := range []string{"redirect", "server-error", "lost-response"} {
		var posts atomic.Int32
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			posts.Add(1)
			switch mode {
			case "redirect":
				w.Header().Set("Location", target.URL)
				w.WriteHeader(307)
			case "server-error":
				w.WriteHeader(500)
				fmt.Fprint(w, `{"ok":false,"error_code":500}`)
			case "lost-response":
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				} else {
					t.Error(err)
				}
			}
		})
		_, err := c.SendProtectedMessage(context.Background(), 42, preparedText)
		var apiErr *APIError
		if err == nil || posts.Load() != 1 || redirected.Load() != 0 || (errors.As(err, &apiErr) && apiErr.Rejected) {
			t.Fatal("ambiguous attempt retried/redirected or classified as rejection")
		}
	}
}

// Marshaling can run caller code; the shared helper must check cancellation
// again immediately before Do, in addition to the protected callback checks.
type cancelingTelegramPayload struct{ cancel context.CancelFunc }

func (p cancelingTelegramPayload) MarshalJSON() ([]byte, error) { p.cancel(); return []byte(`{}`), nil }

func TestTelegramCallRechecksContextBeforeDo(t *testing.T) {
	c := testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("canceled marshal reached network") })
	var calls atomic.Int32
	c.http.Transport = protectedTransport(func(r *http.Request) (*http.Response, error) { calls.Add(1); return http.DefaultTransport.RoundTrip(r) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := c.call(ctx, "sendMessage", cancelingTelegramPayload{cancel: cancel}, new(any))
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatal("missing final context check")
	}
}

func protectedJSONAliasCases(body string, fields map[string]string) map[string]string {
	cases := make(map[string]string)
	for key, value := range fields {
		aliases := []string{key, strings.ToUpper(key)}
		if strings.Contains(key, "s") {
			aliases = append(aliases, strings.Replace(key, "s", "ſ", 1))
		}
		if strings.Contains(key, "k") {
			aliases = append(aliases, strings.Replace(key, "k", "K", 1))
		}
		for _, alias := range aliases {
			cases[alias] = strings.Replace(body, fmt.Sprintf(`%q:`, key), fmt.Sprintf(`%q:%s,%q:`, alias, value, key), 1)
		}
	}
	return cases
}

func TestProtectedJSONUnicodeAliasesAtEveryDepth(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		fields     map[string]string
	}{
		{"success", `{"ok":true,"result":{"message_id":7,"chat":{"id":42,"type":"private"}}}`,
			map[string]string{"ok": "false", "result": "{}", "message_id": "1", "chat": "{}", "id": "99", "type": `"group"`}},
		{"error", `{"ok":false,"error_code":400,"parameters":{"retry_after":1}}`,
			map[string]string{"ok": "true", "error_code": "500", "parameters": "{}", "retry_after": "2"}},
		{"unknown", `{"ok":false,"error_code":400,"unknown":[{"k":1,"s":2}]}`,
			map[string]string{"k": "3", "s": "4"}},
	} {
		for name, body := range protectedJSONAliasCases(tc.body, tc.fields) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				if uniqueTelegramJSON([]byte(body)) {
					t.Fatal("Unicode encoding/json alias not recognized")
				}
				var calls atomic.Int32
				c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					fmt.Fprint(w, body)
				})
				id, err := c.SendProtectedMessage(context.Background(), 42, preparedText)
				var apiErr *APIError
				if err == nil || id != 0 || (errors.As(err, &apiErr) && apiErr.Rejected) || calls.Load() != 1 {
					t.Fatal("ambiguous response accepted, rearmed or replayed", err)
				}
			})
		}
	}
}

func TestProtectedContradictorySuccessIsUnknown(t *testing.T) {
	for _, facts := range []string{
		`"error_code":400`, `"error_code":429`, `"error_code":-1`,
		`"parameters":{"retry_after":1}`, `"parameters":{"retry_after":0}`,
		`"parameters":{"retry_after":null}`, `"parameterſ":{"RETRY_AFTER":0}`,
	} {
		t.Run(facts, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				result := `{"message_id":7,"chat":{"id":42,"type":"private"}}`
				if strings.HasSuffix(r.URL.Path, "/getChat") {
					result = `{"id":42,"type":"private"}`
				}
				fmt.Fprintf(w, `{"ok":true,%s,"result":%s}`, facts, result)
			})
			id, err := c.SendProtectedMessage(context.Background(), 42, preparedText)
			var apiErr *APIError
			if err == nil || id != 0 || (errors.As(err, &apiErr) && apiErr.Rejected) {
				t.Fatal("contradictory positive receipt accepted or rearmed", err)
			}
			if err := c.GetPrivateChat(context.Background(), 42); err == nil {
				t.Fatal("contradictory private-chat success accepted")
			}
			// Ordinary callers retain their historical permissive success policy.
			if id, err := c.SendMessage(context.Background(), 42, "ordinary"); err != nil || id != 7 || calls.Load() != 3 {
				t.Fatal("ordinary send behavior changed or call replayed", err)
			}
		})
	}
}

func TestContradictoryRejectionNeverRearms(t *testing.T) {
	for _, status := range []int{200, 400, 403, 429} {
		for _, key := range []string{"result", "RESULT", "reſult"} {
			for _, result := range []string{`null`, `{}`, `{"message_id":7,"chat":{"id":42,"type":"private"}}`} {
				t.Run(fmt.Sprintf("%d/%s/%s", status, key, result), func(t *testing.T) {
					var calls atomic.Int32
					c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.WriteHeader(status)
						fmt.Fprintf(w, `{"ok":false,"error_code":400,%q:%s}`, key, result)
					})
					for _, protected := range []bool{true, false} {
						var id int64
						var err error
						if protected {
							id, err = c.SendProtectedMessage(context.Background(), 42, preparedText)
						} else {
							id, err = c.SendMessage(context.Background(), 42, "ordinary")
						}
						var apiErr *APIError
						if id != 0 || !errors.As(err, &apiErr) || apiErr.Rejected || apiErr.Code != 400 || apiErr.HTTPStatus != status || !apiErr.Permanent() {
							t.Fatal("contradiction rearmed delivery or changed ordinary error policy", err)
						}
					}
					if calls.Load() != 2 {
						t.Fatal("response caused a replay")
					}
				})
			}
		}
	}
}

func TestProtectedJSONBoundsAndUniqueUnicodeMetadata(t *testing.T) {
	const receipt = `{"ok":true,"result":{"message_id":7,"chat":{"id":42,"type":"private"}}}`
	for name, body := range map[string]string{
		"depth":    receipt[:len(receipt)-1] + `,"unknown":` + strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129) + `}`,
		"trailing": receipt + ` {}`,
		"garbage":  receipt + ` trailing`,
	} {
		t.Run(name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			id, err := c.SendProtectedMessage(context.Background(), 42, preparedText)
			var apiErr *APIError
			if err == nil || id != 0 || (errors.As(err, &apiErr) && apiErr.Rejected) {
				t.Fatal("unbounded/trailing response accepted or rearmed", err)
			}
		})
	}
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{strings.Repeat("[", 128) + "0" + strings.Repeat("]", 128), true},
		{strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129), false},
		{`{"σ":1,"ς":2}`, false},
		{`{"s":1,"\u017f":2}`, false},
		{`{"k":1,"\u212a":2}`, false},
		{`{} null`, false},
	} {
		if uniqueTelegramJSON([]byte(tc.raw)) != tc.valid {
			t.Fatal("incorrect strict JSON depth/fold/EOF validation")
		}
	}
	for _, method := range []string{"sendMessage", "getChat", "error"} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			body := receipt
			if method == "getChat" {
				body = `{"ok":true,"result":{"id":42,"type":"private"}}`
			} else if method == "error" {
				body = `{"ok":false,"error_code":400}`
			}
			fmt.Fprint(w, body[:len(body)-1]+`,"未知":{"ſ":1,"K":2,"é":3,"e":4,"large":1e10000}} `)
		})
		if method == "getChat" {
			if err := c.GetPrivateChat(context.Background(), 42); err != nil {
				t.Fatal("unique Unicode metadata rejected", err)
			}
			continue
		}
		id, err := c.SendProtectedMessage(context.Background(), 42, preparedText)
		if method == "error" {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || !apiErr.Rejected {
				t.Fatal("unique Unicode metadata invalidated known rejection", err)
			}
		} else if err != nil || id != 7 {
			t.Fatal("unique Unicode metadata invalidated success", err)
		}
	}
}
