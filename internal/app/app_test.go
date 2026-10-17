package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	a, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Close() })
	return a
}
func call(a *App, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}
func setupTavily(t *testing.T, a *App, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	_, e := a.db.Exec(`UPDATE providers SET enabled=0`)
	if e != nil {
		t.Fatal(e)
	}
	_, e = a.db.Exec(`UPDATE providers SET enabled=1,base_url=? WHERE id='tavily'`, up.URL)
	if e != nil {
		t.Fatal(e)
	}
	return up
}

const validResult = `{"results":[{"title":"Go HTTP","url":"https://go.dev/doc/","content":"Official documentation"}]}`

func TestRoleIsolationAndStrictJSON(t *testing.T) {
	a := testApp(t)
	for _, tc := range []struct {
		method, path, token, body string
		want                      int
	}{{"GET", "/healthz", "", "", 200}, {"GET", "/api/admin/keys", a.clientToken, "", 401}, {"GET", "/v1/capabilities", a.clientToken, "", 200}, {"POST", "/v1/search", "", `{"query":"q"}`, 401}, {"POST", "/v1/search", a.clientToken, `{"query":"q","unknown":true}`, 400}, {"POST", "/v1/search", a.clientToken, `{"query":"q"} {}`, 400}, {"POST", "/v1/search", a.clientToken, `{"query":"q","providers":["missing"]}`, 400}, {"GET", "/api/admin/keys", a.adminToken, "", 200}} {
		w := call(a, tc.method, tc.path, tc.token, tc.body)
		if w.Code != tc.want {
			t.Errorf("%s %s got %d want %d: %s", tc.method, tc.path, w.Code, tc.want, w.Body)
		}
	}
}
func TestKeyRotationCooldownAndPersistence(t *testing.T) {
	a := testApp(t)
	var mu sync.Mutex
	seen := []string{}
	setupTavily(t, a, func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()
		if key == "Bearer rate-limited-key" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			fmt.Fprint(w, "secret response must never be logged")
			return
		}
		fmt.Fprint(w, validResult)
	})
	bad, e := a.addKey("tavily", "limited", "rate-limited-key", 1, 2)
	if e != nil {
		t.Fatal(e)
	}
	good, e := a.addKey("tavily", "works", "working-private-key", 1, 2)
	if e != nil {
		t.Fatal(e)
	}
	out, e := a.search(context.Background(), SearchInput{Query: "Go", NoCache: true})
	if e != nil || len(out.Results) != 1 || len(out.Attempts) != 2 {
		t.Fatalf("%+v %v", out, e)
	}
	if out.Attempts[0].KeyID != bad || out.Attempts[1].KeyID != good {
		t.Fatal("did not rotate")
	}
	keys, _ := a.keys("tavily")
	if keys[0].CooldownUntil < time.Now().Unix()+100 {
		t.Fatal("Retry-After ignored")
	}
	if !keys[0].Enabled {
		t.Fatal("429 should cool down, not disable")
	}
	if keys[1].Successes != 1 {
		t.Fatal("missing usage accounting")
	}
	w := call(a, "GET", "/api/admin/keys", a.adminToken, "")
	if strings.Contains(w.Body.String(), "working-private-key") || strings.Contains(w.Body.String(), "rate-limited-key") {
		t.Fatal("secret exposed")
	}
	var ciphertext string
	_ = a.db.QueryRow(`SELECT secret FROM api_keys WHERE id=?`, good).Scan(&ciphertext)
	if strings.Contains(ciphertext, "working-private-key") {
		t.Fatal("stored plaintext")
	}
	plain, e := a.decrypt(ciphertext)
	if e != nil || plain != "working-private-key" {
		t.Fatal("encryption roundtrip failed")
	}
	logs := call(a, "GET", "/api/admin/logs", a.adminToken, "")
	if strings.Contains(logs.Body.String(), "secret response") {
		t.Fatal("upstream body leaked")
	}
	a2, e := New(a.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	defer a2.Close()
	restored, _ := a2.keys("tavily")
	if restored[0].CooldownUntil != keys[0].CooldownUntil {
		t.Fatal("cooldown not persisted")
	}
	if !equalToken(a2.clientToken, a.clientToken) {
		t.Fatal("token changed on restart")
	}
}
func TestInvalidKeyDisabledAndServiceFailureBounded(t *testing.T) {
	for _, status := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a := testApp(t)
			calls := 0
			setupTavily(t, a, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(status) })
			_, _ = a.addKey("tavily", "one", "secret-one", 1, 1)
			_, _ = a.addKey("tavily", "two", "secret-two", 1, 1)
			w := call(a, "POST", "/v1/search", a.clientToken, `{"query":"q"}`)
			if w.Code != 502 {
				t.Fatalf("got %d", w.Code)
			}
			keys, _ := a.keys("tavily")
			if status == 500 {
				if calls != 1 {
					t.Fatal("service failure retried all keys")
				}
			} else {
				if calls != 2 || keys[0].Enabled || keys[1].Enabled {
					t.Fatal("invalid keys not disabled/rotated")
				}
			}
		})
	}
}
func TestWeightedRotationAndConcurrency(t *testing.T) {
	a := testApp(t)
	one, _ := a.addKey("tavily", "one", "a", 1, 1)
	two, _ := a.addKey("tavily", "two", "b", 2, 1)
	counts := map[int64]int{}
	for i := 0; i < 30; i++ {
		k, e := a.acquire("tavily", nil)
		if e != nil {
			t.Fatal(e)
		}
		counts[k.ID]++
		a.release(k, 200, 0, "")
	}
	if counts[one] != 10 || counts[two] != 20 {
		t.Fatalf("unexpected weighted distribution %v", counts)
	}
	k1, e := a.acquire("tavily", nil)
	if e != nil {
		t.Fatal(e)
	}
	k2, e := a.acquire("tavily", nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.acquire("tavily", nil); e == nil {
		t.Fatal("concurrency exceeded")
	}
	a.release(k1, 200, 0, "")
	a.release(k2, 200, 0, "")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if k, e := a.acquire("tavily", nil); e == nil {
				time.Sleep(time.Millisecond)
				a.release(k, 200, 0, "")
			}
		}()
	}
	wg.Wait()
	for _, n := range a.flight {
		if n != 0 {
			t.Fatal("leaked in-flight slot")
		}
	}
}
func TestFusionDedupFiltersAndMissingVotes(t *testing.T) {
	rows := map[string][]Hit{"bing": {{Title: "Official", URL: "https://go.dev/doc/?utm_source=x", Snippet: "one"}, {Title: "Same", URL: "https://go.dev/doc/"}}, "duckduckgo": {{Title: "Again", URL: "https://go.dev/doc/", Snippet: "longer official text"}}, "exa": {{Title: "Other", URL: "https://example.com/"}, {Title: "Other two", URL: "https://go.dev/doc/"}}}
	out := fuse(rows, SearchInput{MaxResults: 8, IncludeDomains: []string{"go.dev"}})
	if len(out) != 1 || len(out[0].Providers) != 3 || out[0].Ranks["bing"] != 1 {
		t.Fatalf("bad fusion: %+v", out)
	}
	if out[0].Ranks["exa"] != 2 {
		t.Fatal("original rank lost")
	}
	if len(fuse(rows, SearchInput{MaxResults: 8, ExcludeDomains: []string{"go.dev", "example.com"}})) != 0 {
		t.Fatal("domain exclusion failed")
	}
	if canonical("javascript:alert(1)") != "" {
		t.Fatal("unsafe scheme allowed")
	}
	if !domainMatch("https://docs.go.dev/a", "go.dev") || domainMatch("https://notgo.dev", "go.dev") {
		t.Fatal("domain boundary error")
	}
}
func TestSSRFAndHTMLReader(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "192.168.1.1", "100.64.0.1", "169.254.169.254", "10.1.2.3", "0.0.0.0", "224.0.0.1", "2001:db8::1"} {
		if publicIP(net.ParseIP(ip)) {
			t.Errorf("allowed %s", ip)
		}
	}
	if !publicIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("blocked public IP")
	}
	a := testApp(t)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("SSRF reached local server") }))
	defer local.Close()
	body, _ := json.Marshal(map[string]string{"url": local.URL})
	w := call(a, "POST", "/v1/fetch", a.clientToken, string(body))
	if w.Code != 502 {
		t.Fatalf("expected block, got %d", w.Code)
	}
	text, title := readHTML(`<html><title>Guide</title><nav>menu</nav><main><h1>Welcome</h1><script>stolen()</script><p>Useful content.</p></main></html>`)
	if title != "Guide" || !strings.Contains(text, "Useful content") || strings.Contains(text, "stolen") || strings.Contains(text, "menu") {
		t.Fatalf("bad extraction: %q %q", title, text)
	}
}
func TestCacheAndConfigInvalidation(t *testing.T) {
	a := testApp(t)
	calls := 0
	setupTavily(t, a, func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, validResult) })
	_, _ = a.addKey("tavily", "one", "key", 1, 2)
	in := SearchInput{Query: "go"}
	_, _ = a.search(context.Background(), in)
	out, e := a.search(context.Background(), in)
	if e != nil || !out.CacheHit || calls != 1 {
		t.Fatal("cache miss")
	}
	w := call(a, "POST", "/api/admin/cache/clear", a.adminToken, `{}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	out, _ = a.search(context.Background(), in)
	if out.CacheHit || calls != 2 {
		t.Fatal("cache not invalidated")
	}
}
func TestParserDDGAndCredentialPermissions(t *testing.T) {
	rows := parseDDG([]byte(`<div class="result results_links"><h2><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F">Go docs</a></h2><a class="result__snippet">A useful snippet</a></div>`))
	if len(rows) != 1 || rows[0].URL != "https://go.dev/doc/" || rows[0].Snippet != "A useful snippet" {
		t.Fatalf("bad DDG parse %+v", rows)
	}
	a := testApp(t)
	for _, name := range []string{"master.key", "credentials.json"} {
		info, e := os.Stat(filepath.Join(a.DataDir, name))
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe permissions for %s", name)
		}
	}
	page := call(a, "GET", "/", "", "")
	if page.Code != 200 || !bytes.Contains(page.Body.Bytes(), []byte("搜索工作台")) {
		t.Fatal("embedded UI missing")
	}
}
func TestNoCredentialRedirect(t *testing.T) {
	a := testApp(t)
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	up := setupTavily(t, a, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) })
	_ = up
	_, _ = a.addKey("tavily", "one", "private", 1, 1)
	out, _ := a.search(context.Background(), SearchInput{Query: "q"})
	if leaked || len(out.Results) != 0 {
		t.Fatal("credential-bearing redirect followed")
	}
}
