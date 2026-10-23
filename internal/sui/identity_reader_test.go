package sui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecordReaderListIdentities(t *testing.T) {
	body := `{"success":true,"obj":{"clients":[{"id":9007199254740993,"name":"legacy-123","enable":true},{"id":2,"name":"ppname-456","enable":false}]}}`
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Token") != "identity-secret" {
			t.Errorf("unexpected request method or authentication")
		}
		switch r.URL.RequestURI() {
		case "/panel/apiv2/status?r=sys":
			fmt.Fprint(w, readerStatus)
		case "/panel/apiv2/clients":
			fmt.Fprint(w, body)
		default:
			t.Errorf("unexpected inventory request")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	reader, err := NewIdentityReader(server.URL+"/panel", "identity-secret", "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ListIdentities(context.Background(), 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("inventory failed: %v", err)
	}
	if got[0].ID() != 9007199254740993 || got[0].Name() != "legacy-123" || !got[0].Enabled() || got[1].ID() != 2 || got[1].Name() != "ppname-456" || got[1].Enabled() {
		t.Fatal("identity values changed")
	}
	formatted := fmt.Sprintf("%v %+v %#v", got[0], got[0], got[0])
	if strings.Contains(formatted, "legacy-123") || strings.Contains(formatted, "identity-secret") || slog.AnyValue(got[0]).String() != "[private SUI identity]" {
		t.Fatal("identity formatting disclosed private fields")
	}
	if calls.Load() != 2 {
		t.Fatalf("expected one version and one inventory GET, got %d", calls.Load())
	}
}

func TestRecordReaderListIdentitiesRejectsUntrustedInventory(t *testing.T) {
	valid := `{"id":1,"name":"ppname-1","enable":true}`
	cases := map[string]string{
		"false-success":      `{"success":false,"msg":"identity-secret"}`,
		"missing-list":       `{"success":true,"obj":{}}`,
		"null-list":          `{"success":true,"obj":{"clients":null}}`,
		"duplicate-success":  `{"success":false,"success":true,"obj":{"clients":[]}}`,
		"duplicate-id":       `{"success":true,"obj":{"clients":[` + valid + `,{"id":1,"name":"ppname-2","enable":false}]}}`,
		"duplicate-name":     `{"success":true,"obj":{"clients":[` + valid + `,{"id":2,"name":"ppname-1","enable":false}]}}`,
		"null-id":            `{"success":true,"obj":{"clients":[{"id":null,"name":"ppname-1","enable":true}]}}`,
		"null-name":          `{"success":true,"obj":{"clients":[{"id":1,"name":null,"enable":true}]}}`,
		"null-enable":        `{"success":true,"obj":{"clients":[{"id":1,"name":"ppname-1","enable":null}]}}`,
		"missing-enable":     `{"success":true,"obj":{"clients":[{"id":1,"name":"ppname-1"}]}}`,
		"invalid-id":         `{"success":true,"obj":{"clients":[{"id":0,"name":"ppname-1","enable":true}]}}`,
		"invalid-name":       `{"success":true,"obj":{"clients":[{"id":1,"name":" ppname-1","enable":true}]}}`,
		"wrong-enable":       `{"success":true,"obj":{"clients":[{"id":1,"name":"ppname-1","enable":"true"}]}}`,
		"duplicate-json-key": `{"success":true,"obj":{"clients":[{"id":1,"name":"ppname-1","enable":true,"enable":false}]}}`,
		"oversized":          `{"success":true,"obj":{"clients":[` + valid + `,{"id":2,"name":"ppname-2","enable":false}]}}`,
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			var clientReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("non-GET request issued")
				}
				switch r.URL.Path {
				case "/apiv2/status":
					fmt.Fprint(w, readerStatus)
				case "/apiv2/clients":
					clientReads.Add(1)
					fmt.Fprint(w, response)
				default:
					t.Errorf("unexpected endpoint")
				}
			}))
			defer server.Close()
			reader, err := NewIdentityReader(server.URL, "identity-secret", "1.6.4", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = reader.ListIdentities(context.Background(), 1)
			if err == nil || clientReads.Load() != 1 || strings.Contains(fmt.Sprint(err), "identity-secret") {
				t.Fatal("invalid inventory accepted, retried or disclosed")
			}
		})
	}
}

func TestRecordReaderListIdentitiesWrongVersionAndEmptyList(t *testing.T) {
	for _, tc := range []struct {
		status, clients string
		wantErr         bool
	}{
		{`{"success":true,"obj":{"sys":{"appVersion":"1.6.5"}}}`, `{"success":true,"obj":{"clients":[]}}`, true},
		{readerStatus, `{"success":true,"obj":{"clients":[]}}`, false},
	} {
		var clientReads atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("non-GET request issued")
			}
			if r.URL.Path == "/apiv2/status" {
				fmt.Fprint(w, tc.status)
				return
			}
			clientReads.Add(1)
			fmt.Fprint(w, tc.clients)
		}))
		reader, _ := NewIdentityReader(server.URL, "identity-secret", "1.6.4", time.Second)
		got, err := reader.ListIdentities(context.Background(), 1)
		server.Close()
		if (err != nil) != tc.wantErr || (!tc.wantErr && len(got) != 0) || (tc.wantErr && clientReads.Load() != 0) {
			t.Fatal("version claim or empty inventory contract failed")
		}
	}
}

func TestIdentityReaderValidationBeforeIO(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	for _, version := range []string{"", "1.6.5"} {
		if reader, err := NewIdentityReader(server.URL, "identity-secret", version, time.Second); reader != nil || err == nil {
			t.Fatal("invalid declared version accepted")
		}
	}
	reader, err := NewIdentityReader(server.URL, "identity-secret", "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, maxAccounts := range []int{0, -1, 1001} {
		if _, err := reader.ListIdentities(context.Background(), maxAccounts); err == nil {
			t.Fatal("invalid inventory bound accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid configuration or query performed I/O")
	}
}

func TestIdentityHasNoExportedFieldsOrWriterMethods(t *testing.T) {
	typeOfIdentity := reflect.TypeOf(Identity{})
	for i := 0; i < typeOfIdentity.NumField(); i++ {
		if typeOfIdentity.Field(i).PkgPath == "" {
			t.Fatal("identity data must remain private")
		}
	}
	if reflect.TypeOf((*IdentityReader)(nil)).NumMethod() != 1 || reflect.TypeOf((*IdentityReader)(nil)).Method(0).Name != "ListIdentities" {
		t.Fatal("identity reader must expose only inventory reads")
	}
	if reflect.TypeOf((*RecordReader)(nil)).NumMethod() != 1 || reflect.TypeOf((*RecordReader)(nil)).Method(0).Name != "GetRecord" {
		t.Fatal("record reader must retain its protected single-record capability")
	}
	encoded, err := json.Marshal(Identity{id: 1, name: "private-name", enabled: true})
	if err != nil || strings.Contains(string(encoded), "private-name") {
		t.Fatal("JSON serialization disclosed identity data")
	}
}
