package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const Version = "0.2.4"

type Provider struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	BaseURL    string `json:"base_url"`
	Enabled    bool   `json:"enabled"`
	Timeout    int    `json:"timeout_seconds"`
	Model      string `json:"model"`
	SearchMode string `json:"search_mode"`
}
type Key struct {
	ID            int64  `json:"id"`
	ProviderID    string `json:"provider_id"`
	Label         string `json:"label"`
	Masked        string `json:"masked"`
	Enabled       bool   `json:"enabled"`
	Weight        int    `json:"weight"`
	MaxConcurrent int    `json:"max_concurrent"`
	CooldownUntil int64  `json:"cooldown_until"`
	LastError     string `json:"last_error"`
	Uses          int64  `json:"uses"`
	Successes     int64  `json:"successes"`
	InFlight      int    `json:"in_flight"`
	secret        string
	revision      int64
	lease         *keyLease
}

// A lease is shared by Key copies and consumed under poolMu exactly once.
// Occupancy is per ID across revisions; credential state is revision-scoped.
type keyLease struct {
	released bool
}
type App struct {
	DataDir     string
	db          *sql.DB
	aead        cipher.AEAD
	adminToken  string
	clientToken string
	poolMu      sync.Mutex
	flight      map[int64]int
	turns       map[int64]float64
	cacheMu     sync.Mutex
	cache       map[string]cacheEntry
	searchSlots chan struct{}
	client      *httpClient
}

func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func equalToken(a, b string) bool {
	return len(a) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func privateFile(path string, value []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(value); err != nil {
		return err
	}
	return f.Sync()
}
func New(dir string) (*App, error) {
	if dir == "" {
		dir = "data"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "master.key")
	master, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		master = make([]byte, 32)
		if _, err = rand.Read(master); err != nil {
			return nil, err
		}
		if err = privateFile(keyPath, master); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	credsPath := filepath.Join(dir, "credentials.json")
	var creds struct {
		Admin  string `json:"admin_token"`
		Client string `json:"client_token"`
	}
	data, err := os.ReadFile(credsPath)
	if errors.Is(err, os.ErrNotExist) {
		creds.Admin = token()
		creds.Client = token()
		data, _ = json.MarshalIndent(creds, "", "  ")
		if err = privateFile(credsPath, data); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if err = json.Unmarshal(data, &creds); err != nil {
		return nil, err
	}
	if v := os.Getenv("SAS_ADMIN_TOKEN"); v != "" {
		creds.Admin = v
	}
	if v := os.Getenv("SAS_CLIENT_TOKEN"); v != "" {
		creds.Client = v
	}
	if len(creds.Admin) < 24 || len(creds.Client) < 24 || equalToken(creds.Admin, creds.Client) {
		return nil, errors.New("admin/client tokens must be distinct and at least 24 characters")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "search.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	a := &App{DataDir: dir, db: db, aead: aead, adminToken: creds.Admin, clientToken: creds.Client, flight: map[int64]int{}, turns: map[int64]float64{}, cache: map[string]cacheEntry{}, searchSlots: make(chan struct{}, 16), client: newHTTPClient()}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS providers(id TEXT PRIMARY KEY,name TEXT NOT NULL,kind TEXT NOT NULL,base_url TEXT NOT NULL,enabled INTEGER NOT NULL,timeout_seconds INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS api_keys(id INTEGER PRIMARY KEY AUTOINCREMENT,provider_id TEXT NOT NULL,label TEXT NOT NULL,secret TEXT NOT NULL,masked TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,weight INTEGER NOT NULL DEFAULT 1,max_concurrent INTEGER NOT NULL DEFAULT 2,cooldown_until INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '',uses INTEGER NOT NULL DEFAULT 0,successes INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS requests(id INTEGER PRIMARY KEY AUTOINCREMENT,created_at TEXT NOT NULL,operation TEXT NOT NULL,query TEXT NOT NULL,ok INTEGER NOT NULL,elapsed_ms INTEGER NOT NULL,result_count INTEGER NOT NULL,details TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS requests_created ON requests(created_at);
`)
	if err != nil {
		db.Close()
		return nil, err
	}
	// Additive migration preserves existing providers, key material and settings.
	columns, err := db.Query(`PRAGMA table_info(providers)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	known := map[string]bool{}
	for columns.Next() {
		var id, notnull, pk int
		var name, kind string
		var def any
		if err = columns.Scan(&id, &name, &kind, &notnull, &def, &pk); err != nil {
			columns.Close()
			db.Close()
			return nil, err
		}
		known[name] = true
	}
	columns.Close()
	for _, column := range []string{"model", "search_mode"} {
		if !known[column] {
			if _, err = db.Exec(`ALTER TABLE providers ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	// Close rows before ALTER: SQLite deliberately uses a single connection.
	columns, err = db.Query(`PRAGMA table_info(api_keys)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	hasRevision := false
	for columns.Next() {
		var id, notnull, pk int
		var name, kind string
		var def any
		if err = columns.Scan(&id, &name, &kind, &notnull, &def, &pk); err != nil {
			columns.Close()
			db.Close()
			return nil, err
		}
		hasRevision = hasRevision || name == "revision"
	}
	err = columns.Err()
	columns.Close()
	if err != nil {
		db.Close()
		return nil, err
	}
	if !hasRevision {
		if _, err = db.Exec(`ALTER TABLE api_keys ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`); err != nil {
			db.Close()
			return nil, err
		}
	}
	defaults := []Provider{
		{ID: "bing", Name: "Bing RSS", Kind: "bing", BaseURL: "https://www.bing.com/search", Enabled: true, Timeout: 12},
		{ID: "duckduckgo", Name: "DuckDuckGo", Kind: "duckduckgo", BaseURL: "https://html.duckduckgo.com/html/", Enabled: true, Timeout: 12},
		{ID: "tavily", Name: "Tavily", Kind: "tavily", BaseURL: "https://api.tavily.com", Timeout: 15},
		{ID: "brave", Name: "Brave", Kind: "brave", BaseURL: "https://api.search.brave.com", Timeout: 15},
		{ID: "exa", Name: "Exa", Kind: "exa", BaseURL: "https://api.exa.ai", Timeout: 15},
		{ID: "xai", Name: "Grok / xAI 官方", Kind: "xai", BaseURL: "https://api.x.ai/v1", Timeout: 60, Model: "grok-4.7", SearchMode: "web"},
		{ID: "grok-compatible", Name: "Grok / 兼容中转", Kind: "grok-compatible", BaseURL: "https://api.x.ai/v1", Timeout: 60, Model: "grok-4.7"},
		{ID: "searxng", Name: "SearXNG", Kind: "searxng", BaseURL: "http://localhost:8888", Timeout: 15},
	}
	for _, p := range defaults {
		if _, err = db.Exec(`INSERT OR IGNORE INTO providers(id,name,kind,base_url,enabled,timeout_seconds,model,search_mode) VALUES(?,?,?,?,?,?,?,?)`, p.ID, p.Name, p.Kind, p.BaseURL, p.Enabled, p.Timeout, p.Model, p.SearchMode); err != nil {
			db.Close()
			return nil, err
		}
	}
	return a, nil
}
func (a *App) Close() error { return a.db.Close() }
func (a *App) encrypt(s string) (string, error) {
	nonce := make([]byte, a.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(a.aead.Seal(nonce, nonce, []byte(s), nil)), nil
}
func (a *App) decrypt(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) < a.aead.NonceSize() {
		return "", errors.New("invalid encrypted key")
	}
	n := a.aead.NonceSize()
	v, err := a.aead.Open(nil, b[:n], b[n:], nil)
	return string(v), err
}
func (a *App) providers() ([]Provider, error) {
	rows, err := a.db.Query(`SELECT id,name,kind,base_url,enabled,timeout_seconds,model,search_mode FROM providers ORDER BY CASE WHEN kind='xai' THEN 0 WHEN kind='grok-compatible' THEN 1 ELSE 2 END,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		var p Provider
		if err = rows.Scan(&p.ID, &p.Name, &p.Kind, &p.BaseURL, &p.Enabled, &p.Timeout, &p.Model, &p.SearchMode); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (a *App) keys(provider string) ([]Key, error) {
	q := `SELECT id,provider_id,label,secret,masked,enabled,weight,max_concurrent,cooldown_until,last_error,uses,successes,revision FROM api_keys`
	args := []any{}
	if provider != "" {
		q += " WHERE provider_id=?"
		args = append(args, provider)
	}
	q += " ORDER BY id"
	rows, err := a.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		var k Key
		if err = rows.Scan(&k.ID, &k.ProviderID, &k.Label, &k.secret, &k.Masked, &k.Enabled, &k.Weight, &k.MaxConcurrent, &k.CooldownUntil, &k.LastError, &k.Uses, &k.Successes, &k.revision); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
func (a *App) addKey(provider, label, secret string, weight, concurrency int) (int64, error) {
	ids, err := a.addKeys(provider, []KeyInput{{Label: label, Secret: secret, Weight: weight, MaxConcurrent: concurrency}})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

type KeyInput struct {
	Label         string `json:"label"`
	Secret        string `json:"secret"`
	Weight        int    `json:"weight"`
	MaxConcurrent int    `json:"max_concurrent"`
}

func keyless(kind string) bool { return kind == "bing" || kind == "duckduckgo" || kind == "searxng" }
func maskKey(secret string) string {
	if len(secret) > 4 {
		return "••••" + secret[len(secret)-4:]
	}
	return "••••"
}
func (a *App) addKeys(provider string, input []KeyInput) ([]int64, error) {
	if len(input) < 1 || len(input) > 100 {
		return nil, errors.New("add 1–100 keys per request")
	}
	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var kind string
	if tx.QueryRow(`SELECT kind FROM providers WHERE id=?`, provider).Scan(&kind) != nil {
		return nil, errors.New("provider not found")
	}
	if keyless(kind) {
		return nil, errors.New("this provider does not use keys")
	}
	existing, err := tx.Query(`SELECT secret FROM api_keys WHERE provider_id=?`, provider)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for existing.Next() {
		var encrypted string
		if err = existing.Scan(&encrypted); err != nil {
			existing.Close()
			return nil, err
		}
		plain, e := a.decrypt(encrypted)
		if e != nil {
			existing.Close()
			return nil, e
		}
		seen[plain] = true
	}
	err = existing.Err()
	existing.Close()
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for i, k := range input {
		k.Secret = strings.TrimSpace(k.Secret)
		if k.Secret == "" || len(k.Secret) > 4096 {
			return nil, fmt.Errorf("key %d must contain 1–4096 characters", i+1)
		}
		if seen[k.Secret] {
			return nil, fmt.Errorf("key %d is duplicated in this provider", i+1)
		}
		seen[k.Secret] = true
		if k.Weight == 0 {
			k.Weight = 1
		}
		if k.MaxConcurrent == 0 {
			k.MaxConcurrent = 2
		}
		if k.Weight < 1 || k.Weight > 20 || k.MaxConcurrent < 1 || k.MaxConcurrent > 32 {
			return nil, errors.New("weight must be 1–20; max_concurrent 1–32")
		}
		if k.Label == "" {
			k.Label = "API Key"
		}
		if len(k.Label) > 100 {
			return nil, errors.New("label too long")
		}
		encrypted, e := a.encrypt(k.Secret)
		if e != nil {
			return nil, e
		}
		res, e := tx.Exec(`INSERT INTO api_keys(provider_id,label,secret,masked,weight,max_concurrent) VALUES(?,?,?,?,?,?)`, provider, k.Label, encrypted, maskKey(k.Secret), k.Weight, k.MaxConcurrent)
		if e != nil {
			return nil, e
		}
		id, e := res.LastInsertId()
		if e != nil {
			return nil, e
		}
		ids = append(ids, id)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

var errNoKey = errors.New("no available key (missing, disabled, cooling down or at concurrency limit)")

func (a *App) acquire(provider string, exclude map[int64]bool) (Key, error) {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	keys, err := a.keys(provider)
	if err != nil {
		return Key{}, err
	}
	chosen := -1
	total := 0.0
	for i, k := range keys {
		if !k.Enabled || k.CooldownUntil > time.Now().Unix() || exclude[k.ID] || a.flight[k.ID] >= k.MaxConcurrent {
			continue
		}
		a.turns[k.ID] += float64(k.Weight)
		total += float64(k.Weight)
		if chosen < 0 || a.turns[k.ID] > a.turns[keys[chosen].ID] {
			chosen = i
		}
	}
	if chosen < 0 {
		return Key{}, errNoKey
	}
	k := keys[chosen]
	a.turns[k.ID] -= total
	return a.reserveKey(k)
}

// Caller holds poolMu from reading the credential snapshot through reservation.
func (a *App) reserveKey(k Key) (Key, error) {
	var err error
	k.secret, err = a.decrypt(k.secret)
	if err != nil {
		return Key{}, err
	}
	a.flight[k.ID]++
	_, err = a.db.Exec(`UPDATE api_keys SET uses=uses+1 WHERE id=?`, k.ID)
	if err != nil {
		a.flight[k.ID]--
		return Key{}, err
	}
	k.lease = &keyLease{}
	return k, nil
}
func (a *App) release(k Key, status int, cooldown int64, reason string) {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	if k.lease == nil || k.lease.released {
		return
	}
	k.lease.released = true
	a.flight[k.ID]--
	if a.flight[k.ID] == 0 {
		delete(a.flight, k.ID)
	}
	if status >= 200 && status < 300 {
		// Keep diagnostics while cooling down or disabled by another request.
		_, _ = a.db.Exec(`UPDATE api_keys SET successes=successes+1,last_error=CASE WHEN cooldown_until<=? AND enabled=1 THEN '' ELSE last_error END WHERE id=? AND revision=?`, time.Now().Unix(), k.ID, k.revision)
	} else {
		enabled := status != 401 && status != 403
		// A shorter failure must not overwrite the longer cooldown's diagnosis.
		_, _ = a.db.Exec(`UPDATE api_keys SET enabled=CASE WHEN ? THEN enabled ELSE 0 END,last_error=CASE WHEN ? OR (enabled=1 AND ?>=cooldown_until) THEN ? ELSE last_error END,cooldown_until=MAX(cooldown_until,?) WHERE id=? AND revision=?`, enabled, !enabled, cooldown, reason, cooldown, k.ID, k.revision)
	}
}
func (a *App) record(operation, query string, ok bool, elapsed, count int, details any) {
	b, _ := json.Marshal(details)
	_, _ = a.db.Exec(`INSERT INTO requests(created_at,operation,query,ok,elapsed_ms,result_count,details) VALUES(?,?,?,?,?,?,?)`, time.Now().UTC().Format(time.RFC3339), operation, query, ok, elapsed, count, string(b))
	_, _ = a.db.Exec(`DELETE FROM requests WHERE id < (SELECT COALESCE(MAX(id),0)-2000 FROM requests)`)
}
func (a *App) invalidate() { a.cacheMu.Lock(); a.cache = map[string]cacheEntry{}; a.cacheMu.Unlock() }
func safeError(status int) string {
	if status == 0 {
		return "network request failed"
	}
	return fmt.Sprintf("upstream HTTP %d", status)
}
