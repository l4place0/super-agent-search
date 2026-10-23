package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

func (a *App) requestExtended(ctx context.Context, p Provider, key, query string, count int) ([]Hit, error) {
	u, e := validURL(p.BaseURL)
	if e != nil {
		return nil, e
	}
	method := "POST"
	var payload any
	model := strings.TrimSpace(p.Model)
	if (p.Kind == "xai" || p.Kind == "grok-compatible") && model == "" {
		return nil, errors.New("configure a model for this provider")
	}
	switch p.Kind {
	case "xai":
		u.Path = endpointPath(u.Path, "/responses")
		tools := []map[string]string{}
		if p.SearchMode != "x" {
			tools = append(tools, map[string]string{"type": "web_search"})
		}
		if p.SearchMode == "x" || p.SearchMode == "web_and_x" {
			tools = append(tools, map[string]string{"type": "x_search"})
		}
		payload = map[string]any{"model": model, "input": []map[string]string{{"role": "user", "content": fmt.Sprintf("Search for current public sources for: %s\nReturn a concise answer with source citations; aim for up to %d useful sources. Do not invent source URLs.", query, count)}}, "tools": tools, "stream": false, "store": false}
	case "grok-compatible":
		u.Path = endpointPath(u.Path, "/chat/completions")
		instructions := fmt.Sprintf(`Use web search to find relevant public sources. Current UTC date: %s. Return only a JSON object with this schema: {"results":[{"title":"descriptive source title","url":"original source URL","snippet":"source-specific summary or post text","author":"author name or handle, if known","published_at":"publication date/time as shown by source, if known"}]}. Return up to %d useful sources. For X posts use the original post status URL and retain its author, date and content. Keep each summary associated with its own URL. Follow the user's language. Use empty strings for unknown metadata; never invent sources, dates, quotes or imply model memory was live search. If live search is unavailable or no sources can be retrieved, return {"results":[]}.`, time.Now().UTC().Format("2006-01-02"), count)
		payload = map[string]any{"model": model, "messages": []map[string]string{{"role": "system", "content": instructions}, {"role": "user", "content": query}}, "tools": []map[string]string{{"type": "web_search"}}, "stream": false}
	case "searxng":
		method = "GET"
		u.Path = endpointPath(u.Path, "/search")
		q := u.Query()
		q.Set("q", query)
		q.Set("format", "json")
		u.RawQuery = q.Encode()
	default:
		return nil, errors.New("unsupported provider")
	}
	body, _ := json.Marshal(payload)
	req, e := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if e != nil {
		return nil, errors.New("invalid upstream URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "UniAgentSearch/"+Version)
	if !keyless(p.Kind) {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, e := a.client.upstream.Do(req)
	if e != nil {
		return nil, upstreamError{}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, upstreamError{res.StatusCode, retryUntil(res.Header.Get("Retry-After"))}
	}
	data, e := readBody(res.Body, 4<<20)
	if e != nil {
		return nil, errors.New("upstream response too large or unreadable")
	}
	var hits []Hit
	if p.Kind == "searxng" {
		var response struct {
			Results []struct{ Title, URL, Content string }
		}
		if json.Unmarshal(data, &response) != nil {
			return nil, errors.New("invalid SearXNG JSON; enable the json output format on the instance")
		}
		for _, r := range response.Results {
			hits = append(hits, Hit{Title: r.Title, URL: r.URL, Snippet: r.Content})
		}
	} else {
		hits, e = parseModelSources(data, p.Kind)
		if e != nil {
			return nil, e
		}
	}
	if len(hits) > count {
		hits = hits[:count]
	}
	if len(hits) == 0 {
		return nil, errors.New("no citable sources returned; check search support and model configuration")
	}
	return hits, nil
}
func endpointPath(base, suffix string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, suffix) {
		return base
	}
	return base + suffix
}

// Citations are model-provided candidates, never source-page excerpts. Response
// metadata is preferred; only compatible relays may fall back to Markdown links.
func parseModelSources(data []byte, kind string) ([]Hit, error) {
	var root struct {
		Error     json.RawMessage `json:"error"`
		Status    string          `json:"status"`
		Citations []string        `json:"citations"`
		Output    []struct {
			Type    string `json:"type"`
			Content []struct {
				Type        string                              `json:"type"`
				Text        string                              `json:"text"`
				Annotations []struct{ Type, URL, Title string } `json:"annotations"`
			} `json:"content"`
		} `json:"output"`
		Choices []struct {
			Message struct {
				Content     json.RawMessage `json:"content"`
				Annotations []struct {
					Type        string
					URLCitation struct{ URL, Title string } `json:"url_citation"`
				}
			}
		} `json:"choices"`
	}
	if json.Unmarshal(data, &root) != nil {
		return nil, errors.New("invalid model response")
	}
	if len(root.Error) > 0 && string(root.Error) != "null" {
		return nil, errors.New("model service returned an error")
	}
	if root.Status == "failed" || root.Status == "incomplete" {
		return nil, errors.New("model search did not complete")
	}
	hits := []Hit{}
	seen := map[string]bool{}
	add := func(raw, title string) {
		u := canonical(raw)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		if title == "" || len([]rune(title)) < 3 {
			if parsed, e := url.Parse(u); e == nil {
				title = parsed.Hostname()
			}
		}
		hits = append(hits, Hit{Title: title, URL: u, Snippet: "模型返回的引用来源；请读取来源正文核验。", Basis: "model_citation"})
	}
	for _, item := range root.Output {
		if item.Type != "message" {
			continue
		}
		for _, content := range item.Content {
			if content.Type != "output_text" {
				continue
			}
			for _, ann := range content.Annotations {
				if ann.Type == "url_citation" {
					add(ann.URL, ann.Title)
				}
			}
		}
	}
	for _, citation := range root.Citations {
		add(citation, "")
	}
	if kind == "grok-compatible" {
		for _, choice := range root.Choices {
			for _, ann := range choice.Message.Annotations {
				if ann.Type == "url_citation" {
					add(ann.URLCitation.URL, ann.URLCitation.Title)
				}
			}
			var text string
			if json.Unmarshal(choice.Message.Content, &text) != nil {
				var blocks []struct{ Type, Text string }
				if json.Unmarshal(choice.Message.Content, &blocks) == nil {
					for _, block := range blocks {
						if block.Type == "text" {
							text += block.Text + "\n"
						}
					}
				}
			}
			// Prefer structured per-source content; tolerate fenced JSON from relays.
			structured := strings.TrimSpace(text)
			if strings.HasPrefix(structured, "```") {
				if start := strings.IndexByte(structured, '\n'); start >= 0 {
					structured = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(structured[start+1:]), "```"))
				}
			}
			var result struct {
				Results []Hit `json:"results"`
			}
			if json.Unmarshal([]byte(structured), &result) == nil && result.Results != nil {
				selected := []Hit{}
				selectedURLs := map[string]bool{}
				for _, h := range result.Results {
					add(h.URL, h.Title)
					for i := range hits {
						if hits[i].URL != canonical(h.URL) {
							continue
						}
						if strings.TrimSpace(h.Title) != "" {
							hits[i].Title = h.Title
						}
						if strings.TrimSpace(h.Snippet) != "" {
							hits[i].Snippet = h.Snippet
						}
						hits[i].Author, hits[i].PublishedAt = h.Author, h.PublishedAt
						if !selectedURLs[hits[i].URL] {
							selected = append(selected, hits[i])
							selectedURLs[hits[i].URL] = true
						}
					}
				}
				// Relay citation arrays can include background references. Keep the
				// requested source records ahead of those incidental URLs.
				return selected, nil
			}
			for _, match := range markdownSource.FindAllStringSubmatch(text, -1) {
				add(match[2], strings.Trim(match[1], "[]"))
				// Preserve the source paragraph when the relay ignores JSON format.
				for i := range hits {
					if hits[i].URL != canonical(match[2]) {
						continue
					}
					for _, paragraph := range strings.Split(text, "\n\n") {
						if strings.Contains(paragraph, match[2]) {
							hits[i].Snippet = clip(cleanText(paragraph), 1800)
							break
						}
					}
				}
			}
		}
	}
	return hits, nil
}

var markdownSource = regexp.MustCompile(`\[([^\n]+?)\]\((https?://[^\s<>]+)\)`)
