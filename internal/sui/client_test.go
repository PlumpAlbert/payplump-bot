package sui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

const fixtureToken = "synthetic-fixture-token"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mutateFixture(t *testing.T, name string, mutate func(map[string]any)) []byte {
	t.Helper()
	var envelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(fixture(t, name)))
	decoder.UseNumber() // Mutating a fixture must not round its large integer IDs.
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	mutate(envelope)
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func fixtureClient(t *testing.T, body []byte) *sui.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	client, err := sui.New(server.URL+"/synthetic-panel", fixtureToken, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

var readEndpoints = []struct {
	name    string
	fixture string
	call    func(context.Context, *sui.Client) error
}{
	{"clients", "clients.json", func(ctx context.Context, c *sui.Client) error { _, err := c.ListAccounts(ctx); return err }},
	{"inbounds", "inbounds.json", func(ctx context.Context, c *sui.Client) error { _, err := c.ListInbounds(ctx); return err }},
	{"settings", "settings.json", func(ctx context.Context, c *sui.Client) error { _, err := c.GetSettings(ctx); return err }},
}

func TestReadOnlyRoutesAndTypedResponses(t *testing.T) {
	responses := map[string][]byte{}
	for _, endpoint := range readEndpoints {
		responses["/installed%20panel/apiv2/"+endpoint.name] = fixture(t, endpoint.fixture)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.ContentLength != 0 || r.URL.RawQuery != "" {
			t.Error("expected a GET with no body or query")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Token") != fixtureToken || r.Header.Get("Authorization") != "" || r.Header.Get("X-API-TOKEN") != "" || r.Header.Get("Accept") != "application/json" {
			t.Error("unexpected authentication or content negotiation headers")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, ok := responses[r.URL.EscapedPath()]
		if !ok {
			t.Error("unexpected endpoint or lost installed path prefix")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		seen[r.URL.EscapedPath()]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client, err := sui.New(server.URL+"/installed%20panel/", fixtureToken, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	accounts, err := client.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantAccounts := []sui.Account{
		{ID: 9007199254740993, Name: "synthetic-account-a", Enabled: true, Description: "Synthetic display label", Remark: "", Inbounds: []int64{9007199254740993}, Expiry: 9223372036854775807, Volume: 9007199254740995},
		{ID: 102, Name: "synthetic-account-b", Enabled: false, Description: "", Remark: "Synthetic remark", Inbounds: []int64{}, Expiry: 0, Volume: 0},
	}
	if !reflect.DeepEqual(accounts, wantAccounts) {
		t.Error("account fields, desc/remark mapping, empty arrays or integer precision were not preserved")
	}
	inbounds, err := client.ListInbounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inbounds, []sui.Inbound{{ID: 9007199254740993, Type: "vless", Tag: "synthetic-inbound"}}) {
		t.Error("inbound fields or integer precision were not preserved")
	}
	settings, err := client.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantSettings := sui.Settings{SubURI: "https://subscriptions.invalid/synthetic-prefix/", SubPath: "/synthetic-sub/", SubPort: "12345", SubDomain: "subscriptions.invalid"}
	if settings != wantSettings {
		t.Error("flat settings or string subPort were not preserved")
	}
	encoded, err := json.Marshal(settings)
	if err != nil || bytes.Contains(encoded, []byte("ignored")) {
		t.Error("settings exposed an unmodeled sensitive field")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(responses) {
		t.Error("did not read exactly the expected endpoints")
	}
	for _, count := range seen {
		if count != 1 {
			t.Error("an endpoint was requested more than once")
		}
	}
}

func TestUnsuccessfulHTTP200IsRedacted(t *testing.T) {
	client := fixtureClient(t, fixture(t, "unsuccessful.json"))
	for _, endpoint := range readEndpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			err := endpoint.call(context.Background(), client)
			if err == nil {
				t.Fatal("accepted success:false at HTTP 200")
			}
			for _, private := range []string{"synthetic-sensitive-remote-message", fixtureToken, "synthetic-panel"} {
				if strings.Contains(err.Error(), private) {
					t.Error("error exposed response data, credentials or a configured URL")
				}
			}
		})
	}
}

func TestRejectsInvalidEnvelopes(t *testing.T) {
	bodies := []string{
		"", "{", "null", "[]", `"synthetic"`, `{}`,
		`{"obj":{}}`, `{"success":null,"obj":{}}`,
		`{"success":"true","obj":{}}`, `{"success":1,"obj":{}}`,
		`{"success":true}`, `{"success":true,"obj":null}`,
		`{"success":true,"obj":[]}`, `{"success":true,"obj":123}`,
		`{"success":true,"obj":"synthetic"}`, `{"success":true,"obj":{}}`,
	}
	for i, body := range bodies {
		t.Run(fmt.Sprintf("shape-%d", i), func(t *testing.T) {
			client := fixtureClient(t, []byte(body))
			for _, endpoint := range readEndpoints {
				if err := endpoint.call(context.Background(), client); err == nil {
					t.Errorf("%s accepted an invalid envelope/object", endpoint.name)
				}
			}
		})
	}
}

func TestListsRequireNonNullArrays(t *testing.T) {
	for _, endpoint := range readEndpoints[:2] {
		t.Run(endpoint.name, func(t *testing.T) {
			for i, value := range []any{nil, "synthetic", true, map[string]any{}, []any{nil}} {
				t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
					body := mutateFixture(t, endpoint.fixture, func(envelope map[string]any) {
						envelope["obj"].(map[string]any)[endpoint.name] = value
					})
					if err := endpoint.call(context.Background(), fixtureClient(t, body)); err == nil {
						t.Error("accepted an invalid required list")
					}
				})
			}
			body := mutateFixture(t, endpoint.fixture, func(envelope map[string]any) {
				delete(envelope["obj"].(map[string]any), endpoint.name)
			})
			if err := endpoint.call(context.Background(), fixtureClient(t, body)); err == nil {
				t.Error("accepted a missing required list")
			}
		})
	}
	client := fixtureClient(t, []byte(`{"success":true,"obj":{"clients":[],"inbounds":[]}}`))
	accounts, err := client.ListAccounts(context.Background())
	if err != nil || accounts == nil || len(accounts) != 0 {
		t.Error("did not accept an explicitly empty account list")
	}
	inbounds, err := client.ListInbounds(context.Background())
	if err != nil || inbounds == nil || len(inbounds) != 0 {
		t.Error("did not accept an explicitly empty inbound list")
	}
}

func TestRejectsMalformedAccountFields(t *testing.T) {
	numeric := []any{nil, "123", true, 1.5, json.Number("1e3"), json.Number("9223372036854775808")}
	cases := []struct {
		field   string
		invalid []any
	}{
		{"id", append(append([]any{}, numeric...), 0, -1)},
		{"name", []any{nil, "", 123, false}},
		{"enable", []any{nil, "true", 1}},
		{"desc", []any{nil, 123, false}},
		{"remark", []any{nil, 123, false}},
		{"expiry", numeric},
		{"volume", numeric},
		{"inbounds", []any{nil, "synthetic", map[string]any{}, []any{nil}, []any{0}, []any{-1}, []any{"123"}, []any{1.5}, []any{json.Number("9223372036854775808")}}},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			for i, value := range tc.invalid {
				t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
					body := mutateFixture(t, "clients.json", func(envelope map[string]any) {
						account := envelope["obj"].(map[string]any)["clients"].([]any)[0].(map[string]any)
						account[tc.field] = value
					})
					accounts, err := fixtureClient(t, body).ListAccounts(context.Background())
					if err == nil || accounts != nil {
						t.Error("accepted a malformed account or returned partial results")
					}
				})
			}
			body := mutateFixture(t, "clients.json", func(envelope map[string]any) {
				account := envelope["obj"].(map[string]any)["clients"].([]any)[0].(map[string]any)
				delete(account, tc.field)
			})
			if _, err := fixtureClient(t, body).ListAccounts(context.Background()); err == nil {
				t.Error("accepted a missing required account field")
			}
		})
	}
}

func TestAccountValuesDoNotInferAccessPolicy(t *testing.T) {
	body := mutateFixture(t, "clients.json", func(envelope map[string]any) {
		account := envelope["obj"].(map[string]any)["clients"].([]any)[0].(map[string]any)
		account["expiry"], account["volume"] = -1, -1
	})
	accounts, err := fixtureClient(t, body).ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].Expiry != -1 || accounts[0].Volume != -1 || !accounts[0].Enabled {
		t.Error("adapter interpreted or changed raw account policy fields")
	}
}

func TestRejectsMalformedInboundFields(t *testing.T) {
	cases := []struct {
		field   string
		invalid []any
	}{
		{"id", []any{nil, 0, -1, "123", true, 1.5, json.Number("1e3"), json.Number("9223372036854775808")}},
		{"type", []any{nil, "", 123, false}},
		{"tag", []any{nil, "", 123, false}},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			for i, value := range tc.invalid {
				t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
					body := mutateFixture(t, "inbounds.json", func(envelope map[string]any) {
						inbound := envelope["obj"].(map[string]any)["inbounds"].([]any)[0].(map[string]any)
						inbound[tc.field] = value
					})
					inbounds, err := fixtureClient(t, body).ListInbounds(context.Background())
					if err == nil || inbounds != nil {
						t.Error("accepted a malformed inbound or returned partial results")
					}
				})
			}
			body := mutateFixture(t, "inbounds.json", func(envelope map[string]any) {
				inbound := envelope["obj"].(map[string]any)["inbounds"].([]any)[0].(map[string]any)
				delete(inbound, tc.field)
			})
			if _, err := fixtureClient(t, body).ListInbounds(context.Background()); err == nil {
				t.Error("accepted a missing required inbound field")
			}
		})
	}
}

func TestSettingsRequireFlatStringFields(t *testing.T) {
	wrapped := mutateFixture(t, "settings.json", func(envelope map[string]any) {
		envelope["obj"] = map[string]any{"settings": envelope["obj"]}
	})
	if _, err := fixtureClient(t, wrapped).GetSettings(context.Background()); err == nil {
		t.Error("accepted obj.settings instead of the flat settings map")
	}
	for _, field := range []string{"subURI", "subPath", "subPort", "subDomain"} {
		t.Run(field, func(t *testing.T) {
			for i, value := range []any{nil, 12345, true, []any{}, map[string]any{}} {
				t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
					body := mutateFixture(t, "settings.json", func(envelope map[string]any) {
						envelope["obj"].(map[string]any)[field] = value
					})
					settings, err := fixtureClient(t, body).GetSettings(context.Background())
					if err == nil || settings != (sui.Settings{}) {
						t.Error("accepted a malformed setting or returned partial settings")
					}
				})
			}
			body := mutateFixture(t, "settings.json", func(envelope map[string]any) {
				delete(envelope["obj"].(map[string]any), field)
			})
			if _, err := fixtureClient(t, body).GetSettings(context.Background()); err == nil {
				t.Error("accepted a missing required setting")
			}
		})
	}
	client := fixtureClient(t, []byte(`{"success":true,"obj":{"subURI":"","subPath":"","subPort":"","subDomain":""}}`))
	settings, err := client.GetSettings(context.Background())
	if err != nil || settings != (sui.Settings{}) {
		t.Error("empty but present string settings should remain valid")
	}
}

var invalidBaseURLs = []string{
	"", "ftp://synthetic.invalid/panel", "synthetic.invalid/panel", "//synthetic.invalid/panel", "https:///panel",
	"https://synthetic-user:synthetic-url-secret@synthetic.invalid/panel",
	"https://synthetic.invalid/panel?secret=synthetic-url-secret", "https://synthetic.invalid/panel?",
	"https://synthetic.invalid/panel#synthetic-url-secret", "https://synthetic.invalid/panel#",
	" https://synthetic.invalid/panel", "https://synthetic.invalid/panel ",
	"https://synthetic.invalid/./panel", "https://synthetic.invalid/%2e%2e/panel",
	"https://synthetic.invalid/%0a/panel", "https://synthetic.invalid/%zz/panel",
}

func TestNewRejectsBadConfiguration(t *testing.T) {
	for i, base := range invalidBaseURLs {
		t.Run(fmt.Sprintf("base-%d", i), func(t *testing.T) {
			client, err := sui.New(base, fixtureToken, time.Second)
			assertInvalidConfiguration(t, client != nil, err)
		})
	}
	for i, suffix := range []string{"/apiv2", "/apiv2/", "/apiv2/clients", "/apiv2/inbounds/", "/apiv2/settings", "/%61piv2/clients", "/APIV2", "/apiv2%2Fclients"} {
		t.Run(fmt.Sprintf("suffix-%d", i), func(t *testing.T) {
			client, err := sui.New("https://synthetic.invalid/panel"+suffix, fixtureToken, time.Second)
			assertInvalidConfiguration(t, client != nil, err)
		})
	}
	for i, token := range []string{"", " synthetic-token", "synthetic-token ", "\tsynthetic-token", "synthetic\r\ntoken", "synthetic\x00token", "synthetic\x7ftoken", "synthetic\u0085token", "\u00a0synthetic-token", "synthetic-token\u00a0"} {
		t.Run(fmt.Sprintf("token-%d", i), func(t *testing.T) {
			client, err := sui.New("http://synthetic.netbird.invalid/panel", token, time.Second)
			assertInvalidConfiguration(t, client != nil, err)
		})
	}
	for i, timeout := range []time.Duration{-time.Second, 0, 31 * time.Second} {
		t.Run(fmt.Sprintf("timeout-%d", i), func(t *testing.T) {
			client, err := sui.New("https://synthetic.invalid/panel", fixtureToken, timeout)
			assertInvalidConfiguration(t, client != nil, err)
		})
	}
	for _, base := range []string{"http://synthetic.netbird.invalid", "https://synthetic.invalid/panel/custom/", "https://synthetic.invalid/custom-apiv2"} {
		if _, err := sui.New(base, fixtureToken, time.Second); err != nil {
			t.Error("rejected a supported panel base URL")
		}
	}
}

func assertInvalidConfiguration(t *testing.T, hasResult bool, err error) {
	t.Helper()
	if err == nil || hasResult {
		t.Fatal("accepted invalid configuration")
	}
	if strings.Contains(err.Error(), "synthetic") {
		t.Error("configuration error exposed an input value")
	}
}
