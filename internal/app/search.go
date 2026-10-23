package app

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

type Hit struct {
	Title       string         `json:"title"`
	URL         string         `json:"url"`
	Snippet     string         `json:"snippet"`
	Providers   []string       `json:"providers"`
	Ranks       map[string]int `json:"ranks"`
	Score       float64        `json:"score"`
	Basis       string         `json:"basis"`
	Author      string         `json:"author,omitempty"`
	PublishedAt string         `json:"published_at,omitempty"`
}
type SearchInput struct {
	Query          string   `json:"query"`
	Providers      []string `json:"providers,omitempty"`
	MaxResults     int      `json:"max_results"`
	IncludeDomains []string `json:"include_domains,omitempty"`
	ExcludeDomains []string `json:"exclude_domains,omitempty"`
	NoCache        bool     `json:"no_cache,omitempty"`
}
type Attempt struct {
	Provider string `json:"provider"`
	KeyID    int64  `json:"key_id,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Elapsed  int64  `json:"elapsed_ms"`
	Count    int    `json:"result_count"`
}
type SearchOutput struct {
	Query        string    `json:"query"`
	Results      []Hit     `json:"results"`
	Attempts     []Attempt `json:"attempts"`
	Warnings     []string  `json:"warnings"`
	CacheHit     bool      `json:"cache_hit"`
	Elapsed      int64     `json:"elapsed_ms"`
	ScoreVersion string    `json:"score_version"`
}
type cacheEntry struct {
	value   SearchOutput
	expires time.Time
}
type upstreamError struct {
	status int
	retry  int64
}

func (e upstreamError) Error() string { return safeError(e.status) }
func retryUntil(h string) int64 {
	n, err := strconv.Atoi(h)
	if err == nil && n >= 0 {
		return time.Now().Add(time.Duration(min(n, 86400)) * time.Second).Unix()
	}
	if t, e := http.ParseTime(h); e == nil {
		return min(max(t.Unix(), time.Now().Unix()+1), time.Now().Unix()+86400)
	}
	return time.Now().Add(time.Minute).Unix()
}
func (a *App) requestProvider(ctx context.Context, p Provider, key, query string, count int) ([]Hit, error) {
	if p.Kind == "xai" || p.Kind == "grok-compatible" || p.Kind == "searxng" {
		return a.requestExtended(ctx, p, key, query, count)
	}
	u, err := validURL(p.BaseURL)
	if err != nil {
		return nil, err
	}
	var body any
	method := "GET"
	headers := map[string]string{}
	values := u.Query()
	switch p.Kind {
	case "bing":
		values.Set("q", query)
		values.Set("format", "rss")
		values.Set("count", strconv.Itoa(count))
		u.RawQuery = values.Encode()
	case "duckduckgo":
		values.Set("q", query)
		u.RawQuery = values.Encode()
	case "tavily":
		u.Path = strings.TrimRight(u.Path, "/") + "/search"
		method = "POST"
		body = map[string]any{"query": query, "max_results": count, "include_answer": false}
		headers["Authorization"] = "Bearer " + key
	case "brave":
		u.Path = strings.TrimRight(u.Path, "/") + "/res/v1/web/search"
		values.Set("q", query)
		values.Set("count", strconv.Itoa(count))
		u.RawQuery = values.Encode()
		headers["X-Subscription-Token"] = key
		headers["Cache-Control"] = "no-cache"
	case "exa":
		u.Path = strings.TrimRight(u.Path, "/") + "/search"
		method = "POST"
		body = map[string]any{"query": query, "numResults": count, "type": "auto", "contents": map[string]any{"highlights": true}}
		headers["x-api-key"] = key
	default:
		return nil, errors.New("unsupported provider")
	}
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json,application/xml,text/html")
	req.Header.Set("User-Agent", "UniAgentSearch/"+Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for h, v := range headers {
		req.Header.Set(h, v)
	}
	res, err := a.client.upstream.Do(req)
	if err != nil {
		return nil, upstreamError{}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, upstreamError{res.StatusCode, retryUntil(res.Header.Get("Retry-After"))}
	}
	b, err := readBody(res.Body, 4<<20)
	if err != nil {
		return nil, errors.New("upstream response too large or unreadable")
	}
	hits := []Hit{}
	switch p.Kind {
	case "bing":
		var rss struct {
			Items []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				Description string `xml:"description"`
			} `xml:"channel>item"`
		}
		if err = xml.Unmarshal(b, &rss); err != nil {
			return nil, errors.New("invalid RSS response")
		}
		for _, i := range rss.Items {
			hits = append(hits, Hit{Title: i.Title, URL: i.Link, Snippet: stripHTML(i.Description)})
		}
	case "duckduckgo":
		hits = parseDDG(b)
	case "tavily":
		var data struct {
			Results []struct{ Title, URL, Content string }
		}
		if err = json.Unmarshal(b, &data); err != nil {
			return nil, errors.New("invalid search response")
		}
		for _, i := range data.Results {
			hits = append(hits, Hit{Title: i.Title, URL: i.URL, Snippet: i.Content})
		}
	case "brave":
		var data struct {
			Web struct {
				Results []struct{ Title, URL, Description string }
			}
		}
		if err = json.Unmarshal(b, &data); err != nil {
			return nil, errors.New("invalid search response")
		}
		for _, i := range data.Web.Results {
			hits = append(hits, Hit{Title: i.Title, URL: i.URL, Snippet: stripHTML(i.Description)})
		}
	case "exa":
		var data struct {
			Results []struct {
				Title, URL, Text string
				Highlights       []string
			}
		}
		if err = json.Unmarshal(b, &data); err != nil {
			return nil, errors.New("invalid search response")
		}
		for _, i := range data.Results {
			snippet := strings.Join(i.Highlights, " ")
			if snippet == "" {
				snippet = i.Text
			}
			hits = append(hits, Hit{Title: i.Title, URL: i.URL, Snippet: snippet})
		}
	}
	if len(hits) > count {
		hits = hits[:count]
	}
	if len(hits) == 0 {
		return hits, errors.New("no parsed results (empty response or provider challenge)")
	}
	return hits, nil
}
func stripHTML(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var out strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return cleanText(out.String())
		case html.TextToken:
			out.Write(z.Text())
			out.WriteByte(' ')
		}
	}
}
func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}
func nodeText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(nodeText(c))
		b.WriteByte(' ')
	}
	return cleanText(b.String())
}
func parseDDG(b []byte) []Hit {
	root, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	hits := []Hit{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "div" && strings.Contains(" "+attr(n, "class")+" ", " result ") {
			var h Hit
			var inside func(*html.Node)
			inside = func(c *html.Node) {
				class := " " + attr(c, "class") + " "
				if c.Data == "a" && strings.Contains(class, " result__a ") {
					h.Title = nodeText(c)
					h.URL = attr(c, "href")
				}
				if strings.Contains(class, " result__snippet ") {
					h.Snippet = nodeText(c)
				}
				for x := c.FirstChild; x != nil; x = x.NextSibling {
					inside(x)
				}
			}
			inside(n)
			if strings.HasPrefix(h.URL, "//") {
				h.URL = "https:" + h.URL
			}
			if u, e := url.Parse(h.URL); e == nil && u.Query().Get("uddg") != "" {
				h.URL = u.Query().Get("uddg")
			}
			if h.URL != "" {
				hits = append(hits, h)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return hits
}
func (a *App) providerSearch(ctx context.Context, p Provider, query string, count int) ([]Hit, []Attempt) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.Timeout)*time.Second)
	defer cancel()
	attempts := []Attempt{}
	excluded := map[int64]bool{}
	isKeyless := keyless(p.Kind)
	for tries := 0; tries < 3; tries++ {
		start := time.Now()
		var k Key
		var err error
		if !isKeyless {
			k, err = a.acquire(p.ID, excluded)
			if err != nil {
				attempts = append(attempts, Attempt{Provider: p.ID, Error: err.Error()})
				break
			}
			excluded[k.ID] = true
		}
		hits, err := a.requestProvider(ctx, p, k.secret, query, count)
		attempt := Attempt{Provider: p.ID, KeyID: k.ID, OK: err == nil, Elapsed: time.Since(start).Milliseconds(), Count: len(hits)}
		outcome := classifyProviderOutcome(err, time.Now())
		attempt.Error = outcome.reason
		if err != nil {
			a.metrics.UpstreamFailures.Add(1)
		}
		if !isKeyless {
			if releaseErr := a.release(k, outcome.status, outcome.cooldown, outcome.reason); releaseErr != nil {
				attempt.OK, attempt.Error = false, releaseErr.Error()
				attempts = append(attempts, attempt)
				return nil, attempts
			}
		}
		attempts = append(attempts, attempt)
		if err == nil {
			return hits, attempts
		}
		if isKeyless || !outcome.retry || ctx.Err() != nil {
			break
		}
	}
	return nil, attempts
}
func canonical(raw string) string {
	u, e := validURL(raw)
	if e != nil {
		return ""
	}
	if ip := u.Hostname(); ip == "localhost" || strings.HasSuffix(ip, ".localhost") {
		return ""
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || k == "fbclid" || k == "gclid" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	if u.Path == "/" {
		u.Path = ""
	}
	return u.String()
}
func domainMatch(raw, domain string) bool {
	u, e := url.Parse(raw)
	if e != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return h == d || strings.HasSuffix(h, "."+d)
}
func allowedDomain(raw string, in, out []string) bool {
	for _, d := range out {
		if domainMatch(raw, d) {
			return false
		}
	}
	if len(in) == 0 {
		return true
	}
	for _, d := range in {
		if domainMatch(raw, d) {
			return true
		}
	}
	return false
}
func fuse(rows map[string][]Hit, in SearchInput) []Hit {
	merged := map[string]*Hit{}
	ids := make([]string, 0, len(rows))
	for p := range rows {
		ids = append(ids, p)
	}
	sort.Strings(ids)
	for _, p := range ids {
		seen := map[string]bool{}
		for i, h := range rows[p] {
			u := canonical(h.URL)
			if u == "" || seen[u] || !allowedDomain(u, in.IncludeDomains, in.ExcludeDomains) {
				continue
			}
			seen[u] = true
			r := merged[u]
			if r == nil {
				basis := h.Basis
				if basis == "" {
					basis = "search_extract"
				}
				x := Hit{Title: clip(cleanText(h.Title), 300), URL: u, Snippet: clip(cleanText(h.Snippet), 1800), Author: clip(cleanText(h.Author), 200), PublishedAt: clip(cleanText(h.PublishedAt), 100), Providers: []string{}, Ranks: map[string]int{}, Basis: basis}
				r = &x
				merged[u] = r
			}
			r.Providers = append(r.Providers, p)
			if r.Author == "" {
				r.Author = clip(cleanText(h.Author), 200)
			}
			if r.PublishedAt == "" {
				r.PublishedAt = clip(cleanText(h.PublishedAt), 100)
			}
			r.Ranks[p] = i + 1
			basis := h.Basis
			if basis == "" {
				basis = "search_extract"
			}
			if (r.Basis == "model_citation" && basis == "search_extract") || (r.Basis == basis && len(h.Snippet) > len(r.Snippet)) {
				r.Snippet = clip(cleanText(h.Snippet), 1800)
				r.Basis = basis
			}
		}
	}
	result := []Hit{}
	for _, h := range merged {
		best := 0.0
		sum := 0.0
		group := []float64{}
		for p, r := range h.Ranks {
			v := 60 / float64(60+r)
			if p == "bing" || p == "duckduckgo" {
				group = append(group, v)
			} else {
				sum += v
			}
			best = math.Max(best, v)
		}
		if len(group) > 0 {
			sort.Sort(sort.Reverse(sort.Float64Slice(group)))
			sum += group[0]
			for _, v := range group[1:] {
				sum += 0.25 * v
			}
		}
		h.Score = best + math.Log1p(math.Max(0, sum-best))
		result = append(result, *h)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Score == result[j].Score {
			return result[i].URL < result[j].URL
		}
		return result[i].Score > result[j].Score
	})
	selected := []Hit{}
	domains := map[string]int{}
	for _, h := range result {
		u, _ := url.Parse(h.URL)
		if len(in.IncludeDomains) != 1 && domains[u.Hostname()] >= 3 {
			continue
		}
		domains[u.Hostname()]++
		selected = append(selected, h)
		if len(selected) >= in.MaxResults {
			break
		}
	}
	return selected
}
func (a *App) search(ctx context.Context, in SearchInput) (SearchOutput, error) {
	start := time.Now()
	if a.poolFault.Load() {
		return SearchOutput{}, errStorage
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" || len(in.Query) > 2000 {
		return SearchOutput{}, errors.New("query must contain 1–2000 bytes")
	}
	if in.MaxResults == 0 {
		in.MaxResults = 8
	}
	if in.MaxResults < 1 || in.MaxResults > 20 {
		return SearchOutput{}, errors.New("max_results must be 1–20")
	}
	if len(in.IncludeDomains) > 20 || len(in.ExcludeDomains) > 20 {
		return SearchOutput{}, errors.New("at most 20 domains per filter")
	}
	// Snapshot providers and epoch under the same lock as invalidation. Never
	// hold it over network calls; an old snapshot may only use its old epoch.
	a.cacheMu.Lock()
	providers, err := a.providers()
	epoch := a.cacheEpoch
	a.cacheMu.Unlock()
	if err != nil {
		return SearchOutput{}, err
	}
	requested := map[string]bool{}
	for _, id := range in.Providers {
		requested[id] = true
	}
	selected := []Provider{}
	for _, p := range providers {
		if p.Enabled && (len(in.Providers) == 0 || requested[p.ID]) {
			selected = append(selected, p)
			delete(requested, p.ID)
		}
	}
	if len(requested) > 0 {
		return SearchOutput{}, errors.New("requested provider is unknown or disabled")
	}
	if len(selected) == 0 {
		return SearchOutput{}, errors.New("no enabled providers")
	}
	cacheInput := in
	cacheInput.NoCache = false
	b, _ := json.Marshal(cacheInput)
	cacheKey := string(b)
	if !in.NoCache {
		a.cacheMu.Lock()
		entry, ok := a.cache[cacheKey]
		ok = ok && epoch == a.cacheEpoch
		a.cacheMu.Unlock()
		if ok && time.Now().Before(entry.expires) {
			out := entry.value
			out.CacheHit = true
			out.Elapsed = time.Since(start).Milliseconds()
			if o, ok := ctx.Value(observationKey{}).(*observation); ok {
				o.cacheHit = true
			}
			a.operationRecord(ctx, "search", in.Query, true, int(out.Elapsed), len(out.Results), out.Attempts)
			return out, nil
		}
	}
	rows := map[string][]Hit{}
	out := SearchOutput{Query: in.Query, Attempts: []Attempt{}, Warnings: []string{}, ScoreVersion: "grouped-rank-v1"}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range selected {
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			hits, attempts := a.providerSearch(ctx, p, in.Query, min(20, max(10, in.MaxResults)))
			mu.Lock()
			defer mu.Unlock()
			rows[p.ID] = hits
			out.Attempts = append(out.Attempts, attempts...)
			if len(hits) == 0 {
				out.Warnings = append(out.Warnings, p.ID+": unavailable or empty; see attempts")
			}
		}(p)
	}
	wg.Wait()
	if a.poolFault.Load() {
		return SearchOutput{}, errStorage
	}
	sort.SliceStable(out.Attempts, func(i, j int) bool { return out.Attempts[i].Provider < out.Attempts[j].Provider })
	sort.Strings(out.Warnings)
	out.Results = fuse(rows, in)
	out.Elapsed = time.Since(start).Milliseconds()
	if len(out.Results) > 0 {
		ttl := 5 * time.Minute
		if len(out.Warnings) > 0 {
			ttl = 30 * time.Second
		}
		a.cacheMu.Lock()
		if epoch == a.cacheEpoch {
			if len(a.cache) >= 200 {
				a.cache = map[string]cacheEntry{}
			}
			a.cache[cacheKey] = cacheEntry{out, time.Now().Add(ttl)}
		}
		a.cacheMu.Unlock()
	}
	a.operationRecord(ctx, "search", in.Query, len(out.Results) > 0, int(out.Elapsed), len(out.Results), out.Attempts)
	return out, nil
}
func (a *App) testProvider(ctx context.Context, id string) ([]Attempt, error) {
	ps, err := a.providers()
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.ID == id {
			_, attempts := a.providerSearch(ctx, p, "Go programming language documentation", 3)
			return attempts, nil
		}
	}
	return nil, fmt.Errorf("provider not found")
}
