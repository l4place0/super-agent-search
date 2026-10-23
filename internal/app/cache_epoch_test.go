package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestCacheEpochFencesInFlightAPI(t *testing.T) {
	for _, change := range []string{"clear", "endpoint", "disable"} {
		t.Run(change, func(t *testing.T) {
			a := testApp(t)
			entered, resume := make(chan struct{}), make(chan struct{})
			var enteredOnce, resumeOnce sync.Once
			unblock := func() { resumeOnce.Do(func() { close(resume) }) }
			old := setupTavily(t, a, func(w http.ResponseWriter, r *http.Request) {
				enteredOnce.Do(func() { close(entered) })
				<-resume
				fmt.Fprint(w, validResult)
			})
			defer unblock()
			next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"results":[{"title":"New endpoint","url":"https://example.com/new","content":"new"}]}`)
			}))
			defer next.Close()
			if _, err := a.addKey("tavily", "test", "fake", 1, 2); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- call(a, "POST", "/v1/search", a.clientToken, `{"query":"epoch regression"}`) }()
			<-entered
			if change == "clear" {
				if w := call(a, "POST", "/api/admin/cache/clear", a.adminToken, `{}`); w.Code != 200 {
					t.Fatal(w.Body)
				}
			} else {
				endpoint, enabled := next.URL, true
				if change == "disable" {
					endpoint, enabled = old.URL, false
				}
				body := fmt.Sprintf(`{"name":"Tavily","base_url":%q,"enabled":%t,"timeout_seconds":15}`, endpoint, enabled)
				if w := call(a, "PUT", "/api/admin/providers/tavily", a.adminToken, body); w.Code != 200 {
					t.Fatal(w.Body)
				}
			}
			unblock()
			if w := <-done; w.Code != 200 {
				t.Fatal(w.Body)
			}
			a.cacheMu.Lock()
			n := len(a.cache)
			a.cacheMu.Unlock()
			if n != 0 {
				t.Fatal("old in-flight request repopulated new cache")
			}
			w := call(a, "POST", "/v1/search", a.clientToken, `{"query":"epoch regression"}`)
			if change == "disable" {
				if w.Code != 400 {
					t.Fatalf("disabled provider response: %d %s", w.Code, w.Body)
				}
				return
			}
			var out SearchOutput
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
				t.Fatalf("search: %s %v", w.Body, err)
			}
			if out.CacheHit {
				t.Fatal("new request hit old generation")
			}
			if change == "endpoint" && (len(out.Results) != 1 || out.Results[0].URL != "https://example.com/new") {
				t.Fatalf("old endpoint result: %+v", out)
			}
			w = call(a, "POST", "/v1/search", a.clientToken, `{"query":"epoch regression"}`)
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.CacheHit {
				t.Fatal("new generation did not cache")
			}
		})
	}
}
