package app

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
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadinessAndStorageFailure(t *testing.T) {
	a := testApp(t)
	var logs bytes.Buffer
	a.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	if w := call(a, "GET", "/readyz", "", ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if _, err := a.addKey("tavily", "test", "fake", 1, 1); err != nil {
		t.Fatal(err)
	}
	lease := leaseKey(t, a)
	if _, err := a.db.Exec(`CREATE TRIGGER fail_release BEFORE UPDATE ON api_keys WHEN NEW.successes>OLD.successes BEGIN SELECT RAISE(FAIL,'secret-driver-error'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.release(lease, 200, 0, ""); !errors.Is(err, errStorage) {
		t.Fatal(err)
	}
	if a.flight[lease.ID] != 0 {
		t.Fatal("failed persistence leaked occupancy")
	}
	if _, err := a.acquire("tavily", nil); !errors.Is(err, errStorage) {
		t.Fatal("key pool did not fail closed")
	}
	if w := call(a, "GET", "/readyz", "", ""); w.Code != 503 {
		t.Fatal("unknown key state reported ready")
	}
	if w := call(a, "GET", "/healthz", "", ""); w.Code != 200 {
		t.Fatal("liveness depends on DB")
	}
	a.Close()
	for _, path := range []string{"/readyz", "/api/admin/overview", "/api/admin/logs"} {
		w := call(a, "GET", path, a.adminToken, "")
		if w.Code < 500 {
			t.Fatalf("%s silently succeeded", path)
		}
	}
	w := call(a, "POST", "/v1/search", a.clientToken, `{"query":"test"}`)
	if w.Code != 503 {
		t.Fatal(w.Body)
	}
	if strings.Contains(logs.String(), "secret-driver-error") {
		t.Fatal("SQL driver text leaked")
	}
	if !strings.Contains(logs.String(), "key_release") {
		t.Fatal("missing storage failure log")
	}
}

func TestKeyReadFailureIsNotAnUpstreamFailure(t *testing.T) {
	a := testApp(t)
	setupTavily(t, a, func(w http.ResponseWriter, r *http.Request) { t.Error("broken repository reached upstream") })
	if _, err := a.db.Exec(`DROP TABLE api_keys`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/search", "/api/admin/providers/tavily/test"} {
		body := `{}`
		if path == "/v1/search" {
			body = `{"query":"repository failure"}`
		}
		w := call(a, "POST", path, a.adminToken, body)
		if w.Code != 503 {
			t.Fatalf("repository failure mapped to %d: %s", w.Code, w.Body)
		}
	}
	if a.metrics.UpstreamFailures.Load() != 0 {
		t.Fatal("database fault counted as upstream failure")
	}
}

func TestIngressAccountingIncludesCacheAndFetchFailures(t *testing.T) {
	a := testApp(t)
	a.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	setupTavily(t, a, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, validResult) })
	if _, err := a.addKey("tavily", "test", "fake", 1, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if w := call(a, "POST", "/v1/search", a.clientToken, `{"query":"count cache"}`); w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
	for _, tc := range []struct {
		status   int
		ct, body string
		want     int
	}{
		{503, "text/plain", "private upstream body", 502},
		{200, "application/pdf", "binary", 415},
		{200, "text/html", "<html></html>", 502},
		{200, "text/plain", "useful text", 200},
	} {
		a.client.public.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.ct}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
		})
		if w := call(a, "POST", "/v1/fetch", a.clientToken, `{"url":"https://example.com/"}`); w.Code != tc.want {
			t.Fatal(w.Body)
		}
	}
	if w := call(a, "POST", "/v1/search", "", `{}`); w.Code != 401 {
		t.Fatal(w.Body)
	}
	for i := 0; i < cap(a.searchSlots); i++ {
		a.searchSlots <- struct{}{}
	}
	if w := call(a, "POST", "/v1/search", a.clientToken, `{}`); w.Code != 429 {
		t.Fatal(w.Body)
	}
	for i := 0; i < cap(a.searchSlots); i++ {
		<-a.searchSlots
	}
	var count, success int
	if err := a.db.QueryRow(`SELECT COUNT(*),SUM(ok) FROM requests`).Scan(&count, &success); err != nil {
		t.Fatal(err)
	}
	if count != 8 || success != 3 || a.metrics.Requests.Load() != 8 || a.metrics.CacheHits.Load() != 1 || a.metrics.Rejected.Load() != 1 {
		t.Fatalf("incorrect accounting count=%d success=%d", count, success)
	}
	w := call(a, "GET", "/api/admin/logs", a.adminToken, "")
	if strings.Contains(w.Body.String(), "private upstream body") {
		t.Fatal("upstream body logged")
	}
	if !strings.Contains(w.Body.String(), `"cache_hit":true`) {
		t.Fatal("cache hit not recorded")
	}
}

func TestPanicLogRedactsValuesAndHasRequestID(t *testing.T) {
	a := testApp(t)
	var logs bytes.Buffer
	a.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	handler := a.observe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("private-token-and-provider-body") }))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/fetch?secret=hidden", nil)
	r.Header.Set("Authorization", "Bearer hidden-header")
	r.Header.Set("X-Request-ID", "attacker-id")
	handler.ServeHTTP(w, r)
	if w.Code != 500 || w.Header().Get("X-Request-ID") == "" || w.Header().Get("X-Request-ID") == "attacker-id" {
		t.Fatal("bad panic response")
	}
	for _, secret := range []string{"private-token-and-provider-body", "hidden-header", "secret=hidden"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(w.Body.String(), secret) {
			t.Fatal("panic leaked private data")
		}
	}
	if !strings.Contains(logs.String(), "frames") || !strings.Contains(logs.String(), w.Header().Get("X-Request-ID")) {
		t.Fatal("missing safe diagnostic context")
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.Split(logs.Bytes(), []byte("\n"))[0], &event); err != nil {
		t.Fatal(err)
	}
	if _, err := a.search(context.Background(), SearchInput{}); err == nil {
		t.Fatal("invalid input accepted")
	}
}
