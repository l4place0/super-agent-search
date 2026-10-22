package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBraveSearchAndConnectionTestsSendNoCache(t *testing.T) {
	a := testApp(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") != "no-cache" {
			// Reproduce Brave's validation failure before authentication/search.
			http.Error(w, "invalid cache-control header", http.StatusUnprocessableEntity)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/res/v1/web/search" || r.URL.Query().Get("q") == "" || r.URL.Query().Get("count") == "" {
			t.Errorf("unexpected Brave request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-Subscription-Token") != "test-brave-secret" {
			t.Error("missing Brave subscription token")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"web":{"results":[{"title":"Go docs","url":"https://go.dev/doc/","description":"Official documentation"}]}}`)
	}))
	defer up.Close()
	if _, err := a.db.Exec(`UPDATE providers SET enabled=1,base_url=? WHERE id='brave'`, up.URL); err != nil {
		t.Fatal(err)
	}
	id, err := a.addKey("brave", "test", "test-brave-secret", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, token, body string
	}{
		{"search", "/v1/search", a.clientToken, `{"query":"Go docs","providers":["brave"],"no_cache":true}`},
		{"provider test", "/api/admin/providers/brave/test", a.adminToken, `{}`},
		{"key test", fmt.Sprintf("/api/admin/keys/%d/test", id), a.adminToken, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := call(a, http.MethodPost, tc.path, tc.token, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var attempts []Attempt
			if tc.name == "key test" {
				var attempt Attempt
				if err := json.Unmarshal(w.Body.Bytes(), &attempt); err != nil {
					t.Fatal(err)
				}
				attempts = []Attempt{attempt}
			} else {
				var out SearchOutput
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				attempts = out.Attempts
				if tc.name == "search" && (len(out.Results) != 1 || out.Results[0].URL != "https://go.dev/doc/") {
					t.Fatalf("unexpected results: %+v", out.Results)
				}
			}
			if len(attempts) != 1 || !attempts[0].OK || attempts[0].KeyID != id || attempts[0].Count != 1 {
				t.Fatalf("unexpected attempts: %+v", attempts)
			}
		})
	}
}

func TestExplicitProviderDoesNotSelectRemainingEnabledProviders(t *testing.T) {
	a := testApp(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"[Go](https://go.dev/doc/)"}}]}`)
	}))
	defer up.Close()
	if _, e := a.db.Exec(`UPDATE providers SET enabled=0; UPDATE providers SET enabled=1,base_url=? WHERE id IN ('grok-compatible','bing','duckduckgo')`, up.URL); e != nil {
		t.Fatal(e)
	}
	if _, e := a.addKey("grok-compatible", "test", "test-provider-secret", 1, 2); e != nil {
		t.Fatal(e)
	}
	out, e := a.search(context.Background(), SearchInput{Query: "Go", Providers: []string{"grok-compatible"}, NoCache: true})
	if e != nil || len(out.Attempts) != 1 || out.Attempts[0].Provider != "grok-compatible" || !out.Attempts[0].OK {
		t.Fatalf("explicit provider selection: %+v, %v", out.Attempts, e)
	}
}

func TestGrokProtocolsAndSourceBasis(t *testing.T) {
	a := testApp(t)
	for _, kind := range []string{"xai", "grok-compatible"} {
		t.Run(kind, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-provider-secret" {
					t.Error("missing key")
				}
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Fatal("invalid request")
				}
				if body["model"] != "test-grok-model" || body["stream"] != false {
					t.Error("model/stream mismatch")
				}
				if kind == "xai" {
					if r.URL.Path != "/v1/responses" {
						t.Error(r.URL.Path)
					}
					tools := body["tools"].([]any)
					if len(tools) != 2 || tools[0].(map[string]any)["type"] != "web_search" || tools[1].(map[string]any)["type"] != "x_search" {
						t.Error("wrong tools")
					}
					if _, ok := body["messages"]; ok {
						t.Error("chat fields sent to Responses")
					}
					fmt.Fprint(w, `{"status":"completed","output":[{"type":"web_search_call"},{"type":"message","content":[{"type":"output_text","text":"generated answer","annotations":[{"type":"url_citation","url":"https://go.dev/doc/","title":"Go docs"},{"type":"url_citation","url":"javascript:alert(1)"}]}]}]}`)
				} else {
					if r.URL.Path != "/v1/chat/completions" {
						t.Error(r.URL.Path)
					}
					tools, ok := body["tools"].([]any)
					if !ok || len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search" {
						t.Error("relay must enable only web search")
					}
					fmt.Fprint(w, `{"choices":[{"message":{"content":"See [Go docs](https://go.dev/doc/) and [Go docs](https://go.dev/doc/)."}}]}`)
				}
			}))
			defer up.Close()
			p := Provider{ID: kind, Kind: kind, BaseURL: up.URL + "/v1", Model: "test-grok-model", SearchMode: "web_and_x"}
			hits, e := a.requestProvider(context.Background(), p, "test-provider-secret", "go", 8)
			if e != nil || len(hits) != 1 || hits[0].Basis != "model_citation" {
				t.Fatalf("%+v %v", hits, e)
			}
			fused := fuse(map[string][]Hit{kind: hits}, SearchInput{MaxResults: 8})
			if fused[0].Basis != "model_citation" {
				t.Fatal("model citation mislabeled as page extract")
			}
			fused = fuse(map[string][]Hit{kind: hits, "tavily": {{Title: "Go", URL: "https://go.dev/doc/", Snippet: "short"}}}, SearchInput{MaxResults: 8})
			if fused[0].Basis != "search_extract" || fused[0].Snippet != "short" {
				t.Fatal("did not prefer actual search extract")
			}
		})
	}
	if _, e := parseModelSources([]byte(`{"error":{"message":"secret upstream payload"}}`), "xai"); e == nil || strings.Contains(e.Error(), "secret upstream") {
		t.Fatal("upstream body leaked")
	}
	hits, e := parseModelSources([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"[made up](https://example.com/)"}]}]}`), "xai")
	if e != nil || len(hits) != 0 {
		t.Fatal("official sources should require citation metadata")
	}
}

func TestGrokStructuredSourceContentSurvivesFusion(t *testing.T) {
	content := `{"results":[{"title":"Go release","url":"https://x.com/golang/status/123","snippet":"Go release details","author":"@golang","published_at":"2026-10-16","basis":"fetched_page"},{"title":"Other release","url":"https://x.com/golang/status/456","snippet":"Different release details","author":"@other","published_at":"2026-10-15"},{"title":"unsafe","url":"javascript:alert(1)","snippet":"bad"}]}`
	for _, text := range []string{content, "```json\n" + content + "\n```"} {
		response, _ := json.Marshal(map[string]any{"citations": []string{"https://x.com/unrelated/status/789", "https://x.com/golang/status/123"}, "choices": []any{map[string]any{"message": map[string]any{"content": text}}}})
		hits, e := parseModelSources(response, "grok-compatible")
		if e != nil || len(hits) != 2 {
			t.Fatalf("%+v %v", hits, e)
		}
		out := fuse(map[string][]Hit{"grok-compatible": hits}, SearchInput{MaxResults: 3})
		if len(out) != 2 || out[0].Title != "Go release" || out[0].Snippet != "Go release details" || out[0].Author != "@golang" || out[0].PublishedAt != "2026-10-16" || out[0].Basis != "model_citation" || out[1].Snippet != "Different release details" {
			t.Fatalf("metadata lost or misattributed: %+v", out)
		}
	}
	response, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "First post summary [post](https://x.com/golang/status/123)\n\nSecond post summary [post](https://x.com/golang/status/456)"}}}})
	hits, _ := parseModelSources(response, "grok-compatible")
	if len(hits) != 2 || strings.Contains(hits[0].Snippet, "Second") || !strings.Contains(hits[0].Snippet, "First post summary") || !strings.Contains(hits[1].Snippet, "Second post summary") {
		t.Fatalf("Markdown context: %+v", hits)
	}
}

func TestSearxngKeylessAndEndpoint(t *testing.T) {
	a := testApp(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("q") != "test" || r.Header.Get("Authorization") != "" {
			t.Error("incorrect SearXNG request")
		}
		fmt.Fprint(w, `{"results":[{"title":"Docs","url":"https://go.dev/doc/","content":"source extract"}]}`)
	}))
	defer up.Close()
	p := Provider{ID: "searxng", Kind: "searxng", BaseURL: up.URL + "/search", Timeout: 2}
	hits, attempts := a.providerSearch(context.Background(), p, "test", 5)
	if len(hits) != 1 || len(attempts) != 1 || !attempts[0].OK || attempts[0].KeyID != 0 {
		t.Fatalf("%+v %+v", hits, attempts)
	}
}

func TestProviderScopedBulkKeysAtomicityAndSpecificTest(t *testing.T) {
	a := testApp(t)
	body := `{"keys":[{"label":"one","secret":"key-one"},{"label":"two","secret":"key-two","weight":2}]}`
	w := call(a, "POST", "/api/admin/providers/xai/keys", a.adminToken, body)
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	pool, _ := a.keys("xai")
	if len(pool) != 2 || pool[1].Weight != 2 {
		t.Fatal("bulk add failed")
	}
	w = call(a, "POST", "/api/admin/providers/xai/keys", a.adminToken, `{"keys":[{"secret":"new-key"},{"secret":"key-one"}]}`)
	if w.Code != 400 {
		t.Fatal("duplicate accepted")
	}
	pool, _ = a.keys("xai")
	if len(pool) != 2 {
		t.Fatal("failed batch partially committed")
	}
	if keys, _ := a.keys("brave"); len(keys) != 0 {
		t.Fatal("keys leaked into another provider")
	}
	seen := []string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"citations":["https://go.dev/doc/"]}`)
	}))
	defer up.Close()
	_, _ = a.db.Exec(`UPDATE providers SET base_url=? WHERE id='xai'`, up.URL)
	id := pool[1].ID
	_, _ = a.db.Exec(`UPDATE api_keys SET enabled=0,cooldown_until=9999999999 WHERE id=?`, id)
	w = call(a, "POST", fmt.Sprintf("/api/admin/keys/%d/test", id), a.adminToken, `{}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) || len(seen) != 1 || seen[0] != "Bearer key-two" {
		t.Fatalf("wrong key tested: %s %v", w.Body, seen)
	}
	pool, _ = a.keys("xai")
	if pool[1].Enabled {
		t.Fatal("test unexpectedly enabled disabled key")
	}
	w = call(a, "PUT", fmt.Sprintf("/api/admin/keys/%d", id), a.adminToken, `{"label":"renamed","secret":"replacement-key","enabled":true,"weight":3,"max_concurrent":4}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	pool, _ = a.keys("xai")
	plain, e := a.decrypt(pool[1].secret)
	if e != nil || plain != "replacement-key" || pool[1].Label != "renamed" || pool[1].CooldownUntil != 0 {
		t.Fatal("edit/replace failed")
	}
	if strings.Contains(call(a, "GET", "/api/admin/keys", a.adminToken, "").Body.String(), "replacement-key") {
		t.Fatal("replacement secret leaked")
	}
}

func TestAdditiveProviderMigrationPreservesSettings(t *testing.T) {
	a := testApp(t)
	id, e := a.addKey("tavily", "saved", "persisted-secret", 1, 2)
	if e != nil {
		t.Fatal(e)
	}
	_, e = a.db.Exec(`DELETE FROM providers WHERE id IN ('xai','grok-compatible','searxng'); UPDATE providers SET base_url='https://custom.example.com',timeout_seconds=19,enabled=1 WHERE id='tavily'; ALTER TABLE providers DROP COLUMN model; ALTER TABLE providers DROP COLUMN search_mode;`)
	if e != nil {
		t.Fatal(e)
	}
	a.Close()
	reopened, e := New(a.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	ps, e := reopened.providers()
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, p := range ps {
		if p.ID == "tavily" && (p.BaseURL != "https://custom.example.com" || p.Timeout != 19 || !p.Enabled) {
			t.Fatal("old provider settings overwritten")
		}
		if p.ID == "xai" && p.Model != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("Grok provider not seeded")
	}
	keys, _ := reopened.keys("tavily")
	if len(keys) != 1 || keys[0].ID != id {
		t.Fatal("existing keys changed")
	}
	secret, e := reopened.decrypt(keys[0].secret)
	if e != nil || secret != "persisted-secret" {
		t.Fatal("existing encrypted key unreadable")
	}
}
