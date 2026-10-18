package sui

import (
	"bytes"
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

func recordReaderTestServer(t *testing.T, status, clients string) (*RecordReader, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Token") != "reader-secret" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected method/authentication")
		}
		switch r.URL.EscapedPath() {
		case "/installed%20panel/apiv2/status":
			if r.URL.RawQuery != "r=sys" {
				t.Error("invalid version query")
			}
			fmt.Fprint(w, status)
		case "/installed%20panel/apiv2/clients":
			reads.Add(1)
			if r.URL.RawQuery != "id=123" {
				t.Error("invalid exact-ID query")
			}
			fmt.Fprint(w, clients)
		default:
			t.Error("unexpected endpoint or URL fetch")
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	c, err := NewRecordReader(s.URL+"/installed%20panel", "reader-secret", "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c, &reads
}

const readerStatus = `{"success":true,"obj":{"sys":{"appVersion":"1.6.4"}}}`

func readerClients(raw []byte) string {
	return `{"success":true,"obj":{"clients":[` + string(raw) + `]}}`
}

func TestRecordReaderCapabilityAndValidation(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(RecordReader{}), reflect.TypeOf((*RecordReader)(nil))} {
		for i := 0; i < typ.NumMethod(); i++ {
			if typ.Method(i).Name != "GetRecord" {
				t.Fatalf("unexpected exported method %s", typ.Method(i).Name)
			}
		}
	}
	if reflect.TypeOf((*RecordReader)(nil)).NumMethod() != 1 {
		t.Fatal("reader must expose only GetRecord")
	}
	for _, args := range []struct {
		base, token, version string
		timeout              time.Duration
	}{
		{"http://synthetic.invalid", "reader-secret", "", time.Second},
		{"http://synthetic.invalid", "reader-secret", "1.6.4 ", time.Second},
		{"http://synthetic.invalid", "reader-secret", "1.6.5", time.Second},
		{"http://synthetic.invalid/apiv2", "reader-secret", "1.6.4", time.Second},
		{"http://user:reader-secret@synthetic.invalid", "reader-secret", "1.6.4", time.Second},
		{"http://synthetic.invalid?token=reader-secret", "reader-secret", "1.6.4", time.Second},
		{"http://synthetic.invalid", " reader-secret", "1.6.4", time.Second},
		{"http://synthetic.invalid", "reader-secret\n", "1.6.4", time.Second},
		{"http://synthetic.invalid", "", "1.6.4", time.Second},
		{"http://synthetic.invalid", "reader-secret", "1.6.4", 0},
		{"http://synthetic.invalid", "reader-secret", "1.6.4", 31 * time.Second},
	} {
		c, err := NewRecordReader(args.base, args.token, args.version, args.timeout)
		if c != nil || err == nil || strings.Contains(fmt.Sprint(err), "reader-secret") {
			t.Fatal("invalid configuration accepted or disclosed")
		}
	}
	for _, base := range []string{"http://100.64.0.1/private", "https://synthetic.invalid/installed"} {
		if _, err := NewRecordReader(base, "reader-secret", "v1.6.4", time.Second); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer s.Close()
	c, _ := NewRecordReader(s.URL, "reader-secret", "1.6.4", time.Second)
	for _, id := range []int64{0, -1} {
		if _, err := c.GetRecord(context.Background(), id); err == nil {
			t.Fatal("invalid ID accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid ID performed I/O")
	}
}

func TestRecordReaderFullRecordAndExactVersionSpelling(t *testing.T) {
	for _, version := range []string{"1.6.4", "v1.6.4"} {
		t.Run(version, func(t *testing.T) {
			r := managedTestRecord(t, 9007199254740993)
			r.wire.Links = []map[string]string{{"type": "sub", "uri": "http://must-never-fetch.invalid/secret"}}
			r.wire.Config = json.RawMessage(`{"extension":{"secret":"record-secret","unknown":[1,true,null]}}`)
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.Method != "GET" || req.Header.Get("Token") != "reader-secret" {
					t.Error("wrong read contract")
				}
				switch req.URL.RequestURI() {
				case "/prefix/apiv2/status?r=sys":
					fmt.Fprintf(w, `{"success":true,"obj":{"sys":{"appVersion":%q}}}`, version)
				case "/prefix/apiv2/clients?id=9007199254740993":
					fmt.Fprint(w, readerClients(r.Raw()))
				default:
					t.Error("wrong path, numeric ID or subscription fetch")
					w.WriteHeader(404)
				}
			}))
			defer s.Close()
			c, err := NewRecordReader(s.URL+"/prefix", "reader-secret", version, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				got, err := c.GetRecord(context.Background(), r.ID())
				if err != nil || !bytes.Equal(got.Raw(), r.Raw()) {
					t.Fatal("full snapshot not preserved", err)
				}
				if strings.Contains(fmt.Sprintf("%v %+v %#v", got, got, got), "record-secret") {
					t.Fatal("record formatter disclosed contents")
				}
			}
			if calls.Load() != 4 {
				t.Fatal("missing per-read version probe or unexpected fetch")
			}
		})
	}
}

func TestRecordReaderStrictShapeAndEnvelope(t *testing.T) {
	raw := managedTestRecord(t, 123).Raw()
	cases := map[string]string{
		"unknown":           readerClients(changeRaw(t, raw, func(m map[string]json.RawMessage) { m["future"] = json.RawMessage(`1`) })),
		"wrong-id":          readerClients(changeRaw(t, raw, func(m map[string]json.RawMessage) { m["id"] = json.RawMessage(`124`) })),
		"two-rows":          `{"success":true,"obj":{"clients":[` + string(raw) + `,` + string(raw) + `]}}`,
		"empty":             `{"success":true,"obj":{"clients":[]}}`,
		"summary":           readerClients([]byte(`{"id":123,"name":"summary"}`)),
		"false-200":         `{"success":false,"msg":"reader-secret remote-secret"}`,
		"duplicate-success": `{"success":false,` + readerClients(raw)[1:],
		"duplicate-record":  readerClients(append([]byte(`{"id":123,`), raw[1:]...)),
		"duplicate-config":  readerClients(changeRaw(t, raw, func(m map[string]json.RawMessage) { m["config"] = json.RawMessage(`{"extension":{"a":1,"a":2}}`) })),
		"missing-success":   `{"obj":{"clients":[]}}`,
		"missing-list":      `{"success":true,"obj":{}}`,
	}
	if len(recordKeys) != 21 {
		t.Fatal("expected complete pinned 21-field contract")
	}
	for _, key := range recordKeys {
		cases["missing-"+key] = readerClients(changeRaw(t, raw, func(m map[string]json.RawMessage) { delete(m, key) }))
		cases["null-"+key] = readerClients(changeRaw(t, raw, func(m map[string]json.RawMessage) { m[key] = json.RawMessage(`null`) }))
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, reads := recordReaderTestServer(t, readerStatus, body)
			_, err := c.GetRecord(context.Background(), 123)
			if err == nil || reads.Load() != 1 || strings.Contains(fmt.Sprint(err), "reader-secret") || strings.Contains(fmt.Sprint(err), "remote-secret") {
				t.Fatal("invalid record accepted, retried or disclosed")
			}
		})
	}
}

func TestRecordReaderVersionFailuresBeforeRecordGET(t *testing.T) {
	for _, status := range []string{
		`{"success":true,"obj":{"sys":{"appVersion":"v1.6.4"}}}`,
		`{"success":true,"obj":{"sys":{"appVersion":"1.6.5"}}}`,
		`{"success":true,"obj":{"sys":{"appVersion":"1.6.4+build"}}}`,
		`{"success":true,"obj":{"sys":{}}}`,
		`{"success":false,"msg":"reader-secret"}`,
		`{"success":true,"obj":{"sys":{"appVersion":"bad","appVersion":"1.6.4"}}}`,
	} {
		c, reads := recordReaderTestServer(t, status, "")
		if _, err := c.GetRecord(context.Background(), 123); err == nil || reads.Load() != 0 {
			t.Fatal("bad claim fetched a record")
		}
	}
}

func TestRecordReaderNormalTLSVerification(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	defer s.Close()
	c, err := NewRecordReader(s.URL, "reader-secret", "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetRecord(context.Background(), 123)
	if err == nil || calls.Load() != 0 || strings.Contains(fmt.Sprint(err), "reader-secret") || strings.Contains(fmt.Sprint(err), s.URL) {
		t.Fatal("untrusted TLS certificate accepted or transport error disclosed")
	}
}

func TestRecordReaderNoRedirectRetryAndRedaction(t *testing.T) {
	for _, status := range []int{307, 401, 500} {
		var calls atomic.Int32
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Location", "/must-not-follow/reader-secret")
			w.WriteHeader(status)
			fmt.Fprint(w, "remote-secret")
		}))
		c, _ := NewRecordReader(s.URL, "reader-secret", "1.6.4", time.Second)
		_, err := c.GetRecord(context.Background(), 123)
		if err == nil || calls.Load() != 1 || strings.Contains(fmt.Sprint(err), "reader-secret") || strings.Contains(fmt.Sprint(err), "remote-secret") || strings.Contains(fmt.Sprint(err), s.URL) {
			t.Fatal("failed GET retried, redirected or disclosed")
		}
		s.Close()
	}
}
