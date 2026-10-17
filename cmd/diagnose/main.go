// Server-side support utility. Reads a selected provider key in memory and only
// emits model IDs, HTTP status and redacted diagnostics. Never prints a key.
package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	_ "modernc.org/sqlite"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Println("diagnostic_failed")
		os.Exit(1)
	}
}
func run() error {
	dir := os.Getenv("SAS_DATA_DIR")
	if dir == "" {
		dir = "/data"
	}
	db, e := sql.Open("sqlite", "file:"+filepath.Join(dir, "search.db")+"?mode=ro")
	if e != nil {
		return e
	}
	defer db.Close()
	var base, model, encrypted string
	var id int64
	e = db.QueryRow(`SELECT p.base_url,p.model,k.secret,k.id FROM providers p JOIN api_keys k ON k.provider_id=p.id WHERE p.id='grok-compatible' ORDER BY k.enabled DESC,k.id LIMIT 1`).Scan(&base, &model, &encrypted, &id)
	if e != nil {
		return e
	}
	master, e := os.ReadFile(filepath.Join(dir, "master.key"))
	if e != nil {
		return e
	}
	block, e := aes.NewCipher(master)
	if e != nil {
		return e
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return e
	}
	b, e := base64.StdEncoding.DecodeString(encrypted)
	if e != nil || len(b) < aead.NonceSize() {
		return fmt.Errorf("invalid ciphertext")
	}
	plain, e := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], nil)
	if e != nil {
		return e
	}
	base = strings.TrimSuffix(strings.TrimRight(base, "/"), "/chat/completions")
	for _, route := range []string{"direct", "configured_proxy"} {
		if selected := os.Getenv("SAS_DIAG_ROUTE"); selected != "" && selected != route {
			continue
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if route == "direct" {
			tr.Proxy = nil
		}
		client := &http.Client{Transport: tr, Timeout: 85 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		operation := "models"
		method := "GET"
		var payload []byte
		if os.Getenv("SAS_DIAG_CHAT") == "1" {
			operation = "chat/completions"
			method = "POST"
			tool, query := "web_search", "Go programming language documentation"
			if os.Getenv("SAS_DIAG_X") == "1" {
				query = "Search X for the 3 most recent public posts from @golang about Go releases. Return the exact post text, author handle, publication date, and original https://x.com/.../status/... URL for each. Use live X search. If the tool is unavailable or you cannot retrieve posts, say so; do not invent posts or links."
			}
			payload, _ = json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "system", "content": "Use web search to find relevant public sources. Return only a JSON object with this schema: {\"results\":[{\"title\":\"descriptive source title\",\"url\":\"original source URL\",\"snippet\":\"source-specific summary or post text\",\"author\":\"author name or handle, if known\",\"published_at\":\"publication date/time as shown by source, if known\"}]}. Return up to 10 useful sources. For X posts use the original post status URL and retain its author, date and content. Use empty strings for unknown metadata. Never invent sources."}, {"role": "user", "content": query}}, "tools": []map[string]string{{"type": tool}}, "stream": false})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
		req, e := http.NewRequestWithContext(ctx, method, base+"/"+operation, bytes.NewReader(payload))
		if e != nil {
			cancel()
			return e
		}
		req.Header.Set("Authorization", "Bearer "+string(plain))
		req.Header.Set("Content-Type", "application/json")
		start := time.Now()
		res, e := client.Do(req)
		out := map[string]any{"route": route, "operation": operation, "key_id": id, "model": model, "elapsed_ms": time.Since(start).Milliseconds()}
		if e != nil {
			out["network_error"] = true
		} else {
			out["status"] = res.StatusCode
			body, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
			res.Body.Close()
			var data struct {
				Data  []struct{ ID string }
				Error struct {
					Message string
					Code    any
				}
				Choices []struct{ Message struct{ Content string } }
				Usage   json.RawMessage
			}
			if json.Unmarshal(body, &data) == nil {
				if operation == "models" {
					ids := []string{}
					for _, m := range data.Data {
						ids = append(ids, m.ID)
					}
					out["models"] = ids
				}
				if data.Error.Message != "" {
					msg := strings.ReplaceAll(data.Error.Message, string(plain), "[redacted]")
					msg = regexp.MustCompile(`[A-Za-z0-9_-]{16,}`).ReplaceAllString(msg, "[redacted]")
					if len(msg) > 400 {
						msg = msg[:400]
					}
					out["upstream_error"] = msg
				}
				if len(data.Choices) > 0 {
					out["answer_chars"] = len([]rune(data.Choices[0].Message.Content))
					if os.Getenv("SAS_DIAG_SAMPLE") == "1" {
						out["answer"] = strings.ReplaceAll(data.Choices[0].Message.Content, string(plain), "[redacted]")
						out["usage"] = data.Usage
					}
				}
			}
		}
		cancel()
		encoded, _ := json.Marshal(out)
		fmt.Println(string(encoded))
	}
	return nil
}
