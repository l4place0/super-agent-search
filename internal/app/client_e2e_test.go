package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These execute the actual clients against an offline mock, not just parsers.
func TestSkillClientsAgainstMockHTTP(t *testing.T) {
	for _, client := range []string{"sh", "pwsh"} {
		t.Run(client, func(t *testing.T) {
			if _, err := exec.LookPath(client); err != nil {
				t.Skipf("%s not installed", client)
			}
			if client == "sh" {
				for _, dependency := range []string{"curl", "jq"} {
					if _, err := exec.LookPath(dependency); err != nil {
						t.Skipf("%s not installed", dependency)
					}
				}
			}
			extension := ".sh"
			if client == "pwsh" {
				extension = ".ps1"
			}
			script, err := filepath.Abs("../../skills/uni-agent-search/scripts/uas" + extension)
			if err != nil {
				t.Fatal(err)
			}
			tokenPath := filepath.Join(t.TempDir(), "fake-client-token")
			fakeToken := "test-client-only-not-a-real-secret"
			if err := os.WriteFile(tokenPath, []byte(fakeToken+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name         string
				args         []string
				path         string
				body         map[string]any
				status, exit int
			}{
				{name: "health", args: []string{"health"}, path: "/healthz", status: 200},
				{name: "capabilities", args: []string{"capabilities"}, path: "/v1/capabilities", status: 200},
				{name: "search-unicode-json", args: []string{"search", `中文 "quotes" and \\ paths`, "--max-results", "4", "--providers", "tavily,brave", "--include-domains", "go.dev", "--exclude-domains", "example.com", "--no-cache"}, path: "/v1/search", status: 200, body: map[string]any{"query": `中文 "quotes" and \\ paths`, "max_results": float64(4), "no_cache": true}},
				{name: "fetch-pagination", args: []string{"fetch", "https://example.com/a", "--focus", "Unicode 关键词", "--offset", "100", "--limit", "500"}, path: "/v1/fetch", status: 200, body: map[string]any{"url": "https://example.com/a", "focus": "Unicode 关键词", "offset": float64(100), "limit": float64(500)}},
				{name: "upstream-error", args: []string{"search", "unavailable"}, path: "/v1/search", status: 502, exit: 1},
				{name: "bad-numeric", args: []string{"search", "test", "--max-results", "banana"}, exit: 2},
				{name: "wrong-option", args: []string{"fetch", "https://example.com", "--no-cache"}, exit: 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					received := make(chan bool, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer func() { received <- true }()
						if r.URL.Path != tc.path {
							t.Errorf("path %s != %s", r.URL.Path, tc.path)
						}
						if tc.path != "/healthz" && r.Header.Get("Authorization") != "Bearer "+fakeToken {
							t.Error("client did not use token file precedence")
						}
						if tc.body != nil {
							var payload map[string]any
							if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
								t.Error(err)
							}
							for key, value := range tc.body {
								if payload[key] != value {
									t.Errorf("payload field %s: %v != %v", key, payload[key], value)
								}
							}
							if tc.name == "search-unicode-json" {
								list, ok := payload["providers"].([]any)
								if !ok || len(list) != 2 || list[0] != "tavily" || list[1] != "brave" {
									t.Error("provider array serialization changed")
								}
							}
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(tc.status)
						if tc.status == 502 {
							_, _ = w.Write([]byte(`{"error":"offline mock failure"}`))
						} else {
							_, _ = w.Write([]byte(`{"status":"ok","results":[]}`))
						}
					}))
					defer server.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					args := []string{script}
					if client == "pwsh" {
						args = []string{"-NoProfile", "-File", script}
					}
					args = append(args, tc.args...)
					cmd := exec.CommandContext(ctx, client, args...)
					for _, entry := range os.Environ() {
						if !strings.HasPrefix(entry, "SAS_URL=") && !strings.HasPrefix(entry, "SAS_TOKEN=") && !strings.HasPrefix(entry, "SAS_TOKEN_FILE=") {
							cmd.Env = append(cmd.Env, entry)
						}
					}
					cmd.Env = append(cmd.Env, "SAS_URL="+server.URL, "SAS_TOKEN=wrong-precedence-token", "SAS_TOKEN_FILE="+tokenPath, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
					output, err := cmd.CombinedOutput()
					code := 0
					if err != nil {
						var exit *exec.ExitError
						if !errors.As(err, &exit) {
							t.Fatalf("client launch: %v", err)
						}
						code = exit.ExitCode()
					}
					if code != tc.exit {
						t.Fatalf("exit %d want %d: %s", code, tc.exit, output)
					}
					if strings.Contains(string(output), fakeToken) {
						t.Fatal("client printed token")
					}
					if tc.path != "" {
						select {
						case <-received:
						default:
							t.Fatal("client never called mock HTTP")
						}
					} else {
						select {
						case <-received:
							t.Fatal("invalid input issued HTTP request")
						default:
						}
					}
				})
			}
		})
	}
}
