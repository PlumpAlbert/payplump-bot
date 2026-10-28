package invoiceninja

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func recurringReaderServer(t *testing.T, body []byte, versions ...string) (*RecurringReader, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.EscapedPath() != "/installed%20ninja/api/v1/recurring_invoices/recurring-A" || r.URL.RawQuery != "" {
			t.Error("unexpected path, query or write")
		}
		if r.Header.Get("X-API-TOKEN") != recurringTestToken || r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Accept") != "application/json" {
			t.Error("invalid reader authentication")
		}
		for _, version := range versions {
			w.Header().Add("X-APP-VERSION", version)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	c, err := NewRecurringReader(s.URL+"/installed%20ninja", recurringTestToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

func TestRecurringReaderCapabilityAndValidation(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(RecurringReader{}), reflect.TypeOf((*RecurringReader)(nil))} {
		for i := 0; i < typ.NumMethod(); i++ {
			if typ.Method(i).Name != "Get" {
				t.Fatalf("unexpected exported capability %s", typ.Method(i).Name)
			}
		}
	}
	if reflect.TypeOf((*RecurringReader)(nil)).NumMethod() != 1 {
		t.Fatal("reader must expose only Get")
	}
	for _, args := range []struct {
		base, token string
		timeout     time.Duration
	}{
		{"http://synthetic.invalid/api/v1", recurringTestToken, time.Second},
		{"http://user:secret@synthetic.invalid", recurringTestToken, time.Second},
		{"http://synthetic.invalid?secret=x", recurringTestToken, time.Second},
		{"http://synthetic.invalid", "", time.Second},
		{"http://synthetic.invalid", " secret", time.Second},
		{"http://synthetic.invalid", "secret\n", time.Second},
		{"http://synthetic.invalid", recurringTestToken, 0},
		{"http://synthetic.invalid", recurringTestToken, 31 * time.Second},
	} {
		c, err := NewRecurringReader(args.base, args.token, args.timeout)
		if c != nil || err == nil || strings.Contains(fmt.Sprint(err), recurringTestToken) {
			t.Fatal("invalid configuration accepted or disclosed")
		}
	}
	c, calls := recurringReaderServer(t, nil, SupportedRecurringVersion)
	for _, id := range []string{"", "../recurring", "other/id", " x", strings.Repeat("x", 257)} {
		if _, err := c.Get(context.Background(), id); err == nil {
			t.Fatal("invalid ID accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input caused I/O")
	}
}

func TestRecurringReaderFullForeignHistoryAndUnknownMetadata(t *testing.T) {
	expected := recurringTestRecord(t)
	expected.StatusID, expected.IsDeleted, expected.ArchivedAt = "2", true, 99
	expected.Discount = "2.123456789012345678901"
	expected.LastSentDate = "2040-01-01 08:00:00"
	expected.AutoBill, expected.AutoBillEnabled = "always", true
	expected.LineItems = append(expected.LineItems, expected.LineItems[0])
	fields := recurringTestMap(t, expected)
	fields["future_metadata"] = json.RawMessage(`{"secret":"remote-secret","nested":[true,1]}`)
	body := recurringTestJSON(t, map[string]any{"data": fields, "links": map[string]string{"next": "http://must-never-follow.invalid/secret"}})
	c, calls := recurringReaderServer(t, body, SupportedRecurringVersion)
	got, err := c.Get(context.Background(), expected.ID)
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatal("full strict history changed", err)
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected probe, inventory or URL fetch")
	}
}

func TestRecurringReaderStrictFullPresence(t *testing.T) {
	root := recurringTestMap(t, recurringTestRecord(t))
	line := recurringTestMap(t, recurringTestRecord(t).LineItems[0])
	for _, scope := range []string{"root", "line"} {
		fields := root
		if scope == "line" {
			fields = line
		}
		for key := range fields {
			for _, null := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/null=%t", scope, key, null), func(t *testing.T) {
					data := recurringTestMap(t, recurringTestRecord(t))
					changed := data
					if scope == "line" {
						changed = recurringTestMap(t, recurringTestRecord(t).LineItems[0])
					}
					if null {
						changed[key] = json.RawMessage(`null`)
					} else {
						delete(changed, key)
					}
					if scope == "line" {
						data["line_items"] = recurringTestJSON(t, []any{changed})
					}
					c, calls := recurringReaderServer(t, recurringTestJSON(t, map[string]any{"data": data}), SupportedRecurringVersion)
					if _, err := c.Get(context.Background(), "recurring-A"); err == nil || calls.Load() != 1 {
						t.Fatal("partial record accepted or retried")
					}
				})
			}
		}
	}
}

func TestRecurringReaderVersionAndExactEnvelope(t *testing.T) {
	body := recurringTestJSON(t, map[string]any{"data": recurringTestRecord(t)})
	for _, versions := range [][]string{nil, {"5.13.47"}, {"5.13.50"}, {"5.13.46"}, {"5.13.48"}, {"v5.13.47"}, {"5.13.47+build"}, {SupportedRecurringVersion, SupportedRecurringVersion}} {
		c, calls := recurringReaderServer(t, body, versions...)
		if _, err := c.Get(context.Background(), "recurring-A"); err != nil || calls.Load() != 1 {
			t.Fatalf("release header gated valid record: %v", err)
		}
	}
	wrong := recurringTestRecord(t)
	wrong.ID = "other"
	for _, body := range [][]byte{
		[]byte(`{}`), []byte(`{"data":null}`), []byte(`{"data":[]}`), []byte(`{"data":{"id":"recurring-A"}}`),
		recurringTestJSON(t, map[string]any{"data": wrong}),
		[]byte(`{"data":{"amount":"remote-secret"}}`),
	} {
		c, calls := recurringReaderServer(t, body, SupportedRecurringVersion)
		_, err := c.Get(context.Background(), "recurring-A")
		if err == nil || calls.Load() != 1 || strings.Contains(fmt.Sprint(err), "remote-secret") {
			t.Fatal("bad envelope accepted, retried or disclosed")
		}
	}
}

func TestRecurringReaderNormalTLSVerification(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	defer s.Close()
	c, err := NewRecurringReader(s.URL, recurringTestToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(context.Background(), "recurring-A")
	if err == nil || calls.Load() != 0 || strings.Contains(fmt.Sprint(err), recurringTestToken) || strings.Contains(fmt.Sprint(err), s.URL) {
		t.Fatal("untrusted TLS certificate accepted or transport error disclosed")
	}
}

func TestRecurringReaderNoRedirectRetryAndRedaction(t *testing.T) {
	for _, status := range []int{307, 401, 500} {
		var calls atomic.Int32
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Location", "/must-not-follow/secret")
			w.WriteHeader(status)
			fmt.Fprint(w, "remote-secret")
		}))
		c, _ := NewRecurringReader(s.URL, recurringTestToken, time.Second)
		_, err := c.Get(context.Background(), "recurring-A")
		if err == nil || calls.Load() != 1 || strings.Contains(fmt.Sprint(err), recurringTestToken) || strings.Contains(fmt.Sprint(err), "remote-secret") || strings.Contains(fmt.Sprint(err), s.URL) {
			t.Fatal("failure retried, redirected or disclosed")
		}
		s.Close()
	}
}
