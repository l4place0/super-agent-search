package app

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

//go:embed web/*
var web embed.FS

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	respond(w, status, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		fail(w, 400, "invalid JSON or unsupported fields")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "expected one JSON object")
		return false
	}
	return true
}

func (a *App) auth(admin bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			fail(w, 401, "invalid or missing bearer token")
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		valid := equalToken(token, a.adminToken)
		if !admin {
			valid = valid || equalToken(token, a.clientToken)
		}
		if !valid {
			fail(w, 401, "invalid or missing bearer token")
			return
		}
		next(w, r)
	}
}
func (a *App) slot(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case a.searchSlots <- struct{}{}:
			defer func() { <-a.searchSlots }()
			next(w, r)
		default:
			w.Header().Set("Retry-After", "2")
			fail(w, 429, "server concurrency limit reached")
		}
	}
}
func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"status": "ok", "version": Version})
	})
	m.HandleFunc("GET /v1/capabilities", a.auth(false, func(w http.ResponseWriter, r *http.Request) {
		providers, e := a.providers()
		if e != nil {
			fail(w, 500, "database unavailable")
			return
		}
		respond(w, 200, map[string]any{"version": Version, "operations": []string{"search", "fetch"}, "providers": providers, "max_results": 20, "score_notice": "ranking scores do not verify facts"})
	}))
	m.HandleFunc("POST /v1/search", a.auth(false, a.slot(func(w http.ResponseWriter, r *http.Request) {
		var in SearchInput
		if !decode(w, r, &in) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		out, e := a.search(ctx, in)
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		status := 200
		if len(out.Results) == 0 {
			status = 502
		}
		respond(w, status, out)
	})))
	m.HandleFunc("POST /v1/fetch", a.auth(false, a.slot(a.fetchHandler)))
	m.HandleFunc("GET /api/admin/overview", a.auth(true, a.overview))
	m.HandleFunc("GET /api/admin/providers", a.auth(true, func(w http.ResponseWriter, r *http.Request) {
		p, e := a.providers()
		if e != nil {
			fail(w, 500, "database unavailable")
			return
		}
		respond(w, 200, p)
	}))
	m.HandleFunc("PUT /api/admin/providers/{id}", a.auth(true, a.updateProvider))
	m.HandleFunc("POST /api/admin/providers/{id}/keys", a.auth(true, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Keys []KeyInput `json:"keys"`
		}
		if !decode(w, r, &in) {
			return
		}
		ids, e := a.addKeys(r.PathValue("id"), in.Keys)
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		a.invalidate()
		respond(w, 201, map[string]any{"ids": ids, "added": len(ids)})
	}))
	m.HandleFunc("POST /api/admin/providers/{id}/test", a.auth(true, a.slot(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 80*time.Second)
		defer cancel()
		attempts, e := a.testProvider(ctx, r.PathValue("id"))
		if e != nil {
			fail(w, 404, e.Error())
			return
		}
		respond(w, 200, map[string]any{"attempts": attempts})
	})))
	m.HandleFunc("GET /api/admin/keys", a.auth(true, func(w http.ResponseWriter, r *http.Request) {
		keys, e := a.keys("")
		if e != nil {
			fail(w, 500, "database unavailable")
			return
		}
		a.poolMu.Lock()
		for i := range keys {
			keys[i].InFlight = a.flight[keys[i].ID]
		}
		a.poolMu.Unlock()
		respond(w, 200, keys)
	}))
	m.HandleFunc("POST /api/admin/keys", a.auth(true, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Provider      string `json:"provider_id"`
			Label         string `json:"label"`
			Secret        string `json:"secret"`
			Weight        int    `json:"weight"`
			MaxConcurrent int    `json:"max_concurrent"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Weight == 0 {
			in.Weight = 1
		}
		if in.MaxConcurrent == 0 {
			in.MaxConcurrent = 2
		}
		id, e := a.addKey(in.Provider, in.Label, in.Secret, in.Weight, in.MaxConcurrent)
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		a.invalidate()
		respond(w, 201, map[string]int64{"id": id})
	}))
	m.HandleFunc("PUT /api/admin/keys/{id}", a.auth(true, a.updateKey))
	m.HandleFunc("POST /api/admin/keys/{id}/test", a.auth(true, a.slot(func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil {
			fail(w, 400, "invalid key ID")
			return
		}
		out, e := a.testKey(r.Context(), id)
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		respond(w, 200, out)
	})))
	m.HandleFunc("DELETE /api/admin/keys/{id}", a.auth(true, func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil {
			fail(w, 400, "invalid key ID")
			return
		}
		res, e := a.db.Exec(`DELETE FROM api_keys WHERE id=?`, id)
		if e != nil {
			fail(w, 500, "database unavailable")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			fail(w, 404, "key not found")
			return
		}
		a.invalidate()
		respond(w, 200, map[string]bool{"deleted": true})
	}))
	m.HandleFunc("GET /api/admin/logs", a.auth(true, a.logs))
	m.HandleFunc("POST /api/admin/cache/clear", a.auth(true, func(w http.ResponseWriter, r *http.Request) {
		a.invalidate()
		respond(w, 200, map[string]bool{"cleared": true})
	}))
	sub, _ := fs.Sub(web, "web")
	m.Handle("GET /", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		defer func() {
			if recover() != nil {
				fail(w, 500, "internal server error")
			}
		}()
		m.ServeHTTP(w, r)
	})
}
func (a *App) updateProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string  `json:"name"`
		BaseURL    string  `json:"base_url"`
		Enabled    bool    `json:"enabled"`
		Timeout    int     `json:"timeout_seconds"`
		Model      *string `json:"model"`
		SearchMode *string `json:"search_mode"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, e := validURL(in.BaseURL)
	if e != nil || u.RawQuery != "" || u.Fragment != "" {
		fail(w, 400, "base_url must be an HTTP(S) URL without credentials, query or fragment")
		return
	}
	if len(in.Name) < 1 || len(in.Name) > 80 || in.Timeout < 2 || in.Timeout > 75 {
		fail(w, 400, "name required; timeout_seconds must be 2–75")
		return
	}
	if in.Model != nil && len(*in.Model) > 200 {
		fail(w, 400, "model too long")
		return
	}
	if in.SearchMode != nil && *in.SearchMode != "" && *in.SearchMode != "web" && *in.SearchMode != "web_and_x" && *in.SearchMode != "x" {
		fail(w, 400, "invalid search_mode")
		return
	}
	res, e := a.db.Exec(`UPDATE providers SET name=?,base_url=?,enabled=?,timeout_seconds=?,model=COALESCE(?,model),search_mode=COALESCE(?,search_mode) WHERE id=?`, in.Name, strings.TrimRight(in.BaseURL, "/"), in.Enabled, in.Timeout, in.Model, in.SearchMode, r.PathValue("id"))
	if e != nil {
		fail(w, 500, "database unavailable")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fail(w, 404, "provider not found")
		return
	}
	a.invalidate()
	respond(w, 200, map[string]bool{"saved": true})
}
func (a *App) updateKey(w http.ResponseWriter, r *http.Request) {
	id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if e != nil {
		fail(w, 400, "invalid key ID")
		return
	}
	var in struct {
		Enabled       bool    `json:"enabled"`
		Weight        int     `json:"weight"`
		MaxConcurrent int     `json:"max_concurrent"`
		Reset         bool    `json:"reset_cooldown"`
		Label         *string `json:"label"`
		Secret        string  `json:"secret"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Weight < 1 || in.Weight > 20 || in.MaxConcurrent < 1 || in.MaxConcurrent > 32 {
		fail(w, 400, "invalid weight or concurrency")
		return
	}
	if in.Label != nil && (len(strings.TrimSpace(*in.Label)) == 0 || len(*in.Label) > 100) {
		fail(w, 400, "label must contain 1–100 characters")
		return
	}
	var encrypted, masked *string
	if in.Secret != "" {
		secret := strings.TrimSpace(in.Secret)
		if secret == "" || len(secret) > 4096 {
			fail(w, 400, "invalid replacement key")
			return
		}
		enc, err := a.encrypt(secret)
		if err != nil {
			fail(w, 500, "key encryption failed")
			return
		}
		mask := maskKey(secret)
		encrypted = &enc
		masked = &mask
		in.Reset = true
	}
	res, e := a.db.Exec(`UPDATE api_keys SET enabled=?,weight=?,max_concurrent=?,cooldown_until=CASE WHEN ? THEN 0 ELSE cooldown_until END,last_error=CASE WHEN ? THEN '' ELSE last_error END,label=COALESCE(?,label),secret=COALESCE(?,secret),masked=COALESCE(?,masked) WHERE id=?`, in.Enabled, in.Weight, in.MaxConcurrent, in.Reset, in.Reset, in.Label, encrypted, masked, id)
	if e != nil {
		fail(w, 500, "database unavailable")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fail(w, 404, "key not found")
		return
	}
	a.invalidate()
	respond(w, 200, map[string]bool{"saved": true})
}
func (a *App) overview(w http.ResponseWriter, r *http.Request) {
	var total, success, avg int
	_ = a.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(ok),0),CAST(COALESCE(AVG(elapsed_ms),0) AS INTEGER) FROM requests`).Scan(&total, &success, &avg)
	var keys int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE enabled=1`).Scan(&keys)
	a.cacheMu.Lock()
	cache := len(a.cache)
	a.cacheMu.Unlock()
	respond(w, 200, map[string]any{"version": Version, "requests": total, "successes": success, "average_ms": avg, "active_keys": keys, "cached_searches": cache, "log_limit": 2000})
}
func (a *App) logs(w http.ResponseWriter, r *http.Request) {
	rows, e := a.db.Query(`SELECT id,created_at,operation,query,ok,elapsed_ms,result_count,details FROM requests ORDER BY id DESC LIMIT 60`)
	if e != nil {
		fail(w, 500, "database unavailable")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, elapsed, count int
		var created, op, q, details string
		var ok bool
		if rows.Scan(&id, &created, &op, &q, &ok, &elapsed, &count, &details) != nil {
			continue
		}
		var data any
		_ = json.Unmarshal([]byte(details), &data)
		out = append(out, map[string]any{"id": id, "created_at": created, "operation": op, "query": q, "ok": ok, "elapsed_ms": elapsed, "result_count": count, "details": data})
	}
	respond(w, 200, out)
}

type fetchInput struct {
	URL    string `json:"url"`
	Focus  string `json:"focus,omitempty"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (a *App) fetchHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var in fetchInput
	if !decode(w, r, &in) {
		return
	}
	if _, e := validURL(in.URL); e != nil || len(in.URL) > 4000 || len(in.Focus) > 500 || in.Offset < 0 {
		fail(w, 400, "invalid URL, focus or offset")
		return
	}
	if in.Limit == 0 {
		in.Limit = 16000
	}
	if in.Limit < 100 || in.Limit > 50000 {
		fail(w, 400, "limit must be 100–50000 characters")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", in.URL, nil)
	if e != nil {
		fail(w, 400, "invalid URL")
		return
	}
	req.Header.Set("User-Agent", "SuperAgentSearch/"+Version)
	res, e := a.client.public.Do(req)
	if e != nil {
		a.record("fetch", in.URL, false, int(time.Since(start).Milliseconds()), 0, map[string]string{"error": "public page fetch failed"})
		fail(w, 502, "public page fetch failed; private/reserved networks are blocked")
		return
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		fail(w, 502, safeError(res.StatusCode))
		return
	}
	ct := strings.ToLower(res.Header.Get("Content-Type"))
	if !strings.Contains(ct, "text/") && !strings.Contains(ct, "json") && !strings.Contains(ct, "xml") {
		fail(w, 415, "MVP supports HTML and text pages; PDF and other binaries are unsupported")
		return
	}
	body, e := readBody(res.Body, 4<<20)
	if e != nil {
		fail(w, 502, e.Error())
		return
	}
	content, title := string(body), ""
	if strings.Contains(ct, "html") {
		content, title = readHTML(string(body))
	}
	if strings.TrimSpace(content) == "" {
		fail(w, 502, "no readable page text")
		return
	}
	focusMiss := false
	if in.Focus != "" {
		paragraphs := strings.Split(content, "\n\n")
		selected := []string{}
		terms := strings.Fields(strings.ToLower(in.Focus))
		for _, p := range paragraphs {
			for _, term := range terms {
				if strings.Contains(strings.ToLower(p), term) {
					selected = append(selected, p)
					break
				}
			}
		}
		if len(selected) > 0 {
			content = strings.Join(selected, "\n\n")
		} else {
			focusMiss = true
		}
	}
	chars := []rune(content)
	offset := min(in.Offset, len(chars))
	end := min(len(chars), offset+in.Limit)
	out := map[string]any{"url": res.Request.URL.String(), "title": title, "content": string(chars[offset:end]), "basis": "fetched_page", "fetched_at": time.Now().UTC().Format(time.RFC3339), "total_chars": len(chars), "offset": offset, "truncated": end < len(chars), "focus_miss": focusMiss, "elapsed_ms": time.Since(start).Milliseconds()}
	if end < len(chars) {
		out["next_offset"] = end
	}
	a.record("fetch", in.URL, true, int(time.Since(start).Milliseconds()), 1, map[string]any{"characters": len(chars)})
	respond(w, 200, out)
}
func readHTML(raw string) (string, string) {
	root, e := html.Parse(strings.NewReader(raw))
	if e != nil {
		return "", ""
	}
	var b strings.Builder
	title := ""
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "svg", "nav", "footer", "header", "form", "iframe":
				return
			case "title":
				title = nodeText(n)
				return
			}
			if attr(n, "hidden") != "" || attr(n, "aria-hidden") == "true" {
				return
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "p", "div", "article", "section", "main", "li", "h1", "h2", "h3", "pre", "tr", "blockquote":
				b.WriteString("\n\n")
			}
		}
	}
	walk(root)
	parts := []string{}
	for _, p := range strings.Split(b.String(), "\n\n") {
		if s := cleanText(p); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n"), title
}
