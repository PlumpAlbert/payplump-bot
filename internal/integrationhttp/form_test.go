package integrationhttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExplicitWriterPostsFormWithoutJSONWrapper(t *testing.T) {
	for _, name := range []string{"Post", "PostForm", "requestType"} {
		if _, exposed := reflect.TypeOf((*Client)(nil)).MethodByName(name); exposed {
			t.Fatal("read-only transport exposes mutation capability")
		}
	}
	var requests atomic.Int32
	data := `{"name":"synthetic + & = % / имя","config":{"password":"synthetic-secret"}}`
	want := url.Values{"object": {"clients"}, "action": {"new"}, "data": {data}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "POST" || r.URL.EscapedPath() != "/private%20panel/apiv2/save" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Token") != "synthetic-token" || r.Header.Get("Accept") != "application/json" {
			t.Error("wrong form request")
		}
		if err := r.ParseForm(); err != nil || !reflect.DeepEqual(r.PostForm, want) {
			t.Error("full JSON document was not losslessly form-encoded")
		}
		_, _ = io.WriteString(w, `{"success":true,"obj":{"clients":[]}}`)
	}))
	defer server.Close()
	writer, err := NewWriter(server.URL+"/private%20panel", http.Header{"Token": {"synthetic-token"}}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Success bool `json:"success"`
	}
	_, err = writer.PostForm(context.Background(), []string{"apiv2", "save"}, nil, want, &result)
	if err != nil || !result.Success || requests.Load() != 1 {
		t.Fatal("form write failed or replayed")
	}
}

type formRoundTrip func(*http.Request) (*http.Response, error)

func (f formRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestFormWriterFreezesPayloadAndDisablesBodyReplay(t *testing.T) {
	writer, err := NewWriter("https://synthetic.invalid", nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	payload := url.Values{"data": {"before-network"}}
	var calls int
	writer.client.http.Transport = formRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.GetBody != nil {
			t.Error("form body can be replayed")
		}
		payload.Set("data", "changed-after-encoding")
		body, _ := io.ReadAll(r.Body)
		if string(body) != "data=before-network" {
			t.Error("caller mutation changed an in-flight form")
		}
		return nil, errors.New("never expose this private transport detail")
	})
	var result any
	_, err = writer.PostForm(context.Background(), []string{"save"}, nil, payload, &result)
	if err == nil || calls != 1 || strings.Contains(err.Error(), "private transport detail") {
		t.Fatal("unsafe form error/retry")
	}
}

func TestFormWriterValidatesBeforeHTTPAndRejectsRedirect(t *testing.T) {
	var calls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("form credentials forwarded") }))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", foreign.URL)
		w.WriteHeader(307)
		_, _ = io.WriteString(w, `{"secret":"private-body"}`)
	}))
	defer server.Close()
	writer, err := NewWriter(server.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var result any
	for _, payload := range []url.Values{nil, {}, {"data": {strings.Repeat("&", MaxResponseBytes)}}} {
		if _, err := writer.PostForm(context.Background(), []string{"save"}, nil, payload, &result); err == nil {
			t.Error("empty or oversized form accepted")
		}
	}
	if _, err := writer.PostForm(context.Background(), []string{"save"}, nil, url.Values{"data": {"x"}}, nil); err == nil {
		t.Error("nil destination accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid form reached HTTP")
	}
	_, err = writer.PostForm(context.Background(), []string{"save"}, nil, url.Values{"data": {"x"}}, &result)
	if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "private-body") || strings.Contains(err.Error(), server.URL) {
		t.Fatal("redirect followed or private fields leaked")
	}
}

func TestFormWriterCancellationNeverReplaysAcceptedMutation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-release
	}))
	defer server.Close()
	defer close(release)
	writer, err := NewWriter(server.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var result any
		_, err := writer.PostForm(ctx, []string{"save"}, nil, url.Values{"data": {"synthetic"}}, &result)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("form was not received")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatal("unknown accepted form was replayed")
		}
	case <-time.After(time.Second):
		t.Fatal("form did not cancel")
	}
}
