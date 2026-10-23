package app

import (
	"context"
	"errors"
	"time"
)

// A manual test targets exactly the selected key, including disabled/cooling
// keys. It never enables the key or changes provider configuration implicitly.
func (a *App) acquireTestKey(id int64) (Key, Provider, error) {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	all, e := a.keys("")
	if e != nil {
		return Key{}, Provider{}, e
	}
	var k Key
	found := false
	for _, candidate := range all {
		if candidate.ID == id {
			k = candidate
			found = true
			break
		}
	}
	if !found {
		return Key{}, Provider{}, errors.New("key not found")
	}
	ps, e := a.providers()
	if e != nil {
		return Key{}, Provider{}, e
	}
	var provider Provider
	for _, p := range ps {
		if p.ID == k.ProviderID {
			provider = p
			break
		}
	}
	if provider.ID == "" {
		return Key{}, Provider{}, errors.New("provider not found")
	}
	if a.flight[id] >= k.MaxConcurrent {
		return Key{}, Provider{}, errors.New("key is at its concurrency limit")
	}
	k, e = a.reserveKey(k)
	return k, provider, e
}

func (a *App) testKey(ctx context.Context, id int64) (Attempt, error) {
	k, provider, e := a.acquireTestKey(id)
	if e != nil {
		return Attempt{}, e
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(provider.Timeout)*time.Second)
	defer cancel()
	start := time.Now()
	hits, e := a.requestProvider(ctx, provider, k.secret, "Go programming language documentation", 3)
	out := Attempt{Provider: provider.ID, KeyID: id, OK: e == nil, Count: len(hits), Elapsed: time.Since(start).Milliseconds()}
	status := 200
	cooldown := int64(0)
	if e != nil {
		out.Error = e.Error()
		status = 0
		var ue upstreamError
		if errors.As(e, &ue) {
			status = ue.status
			if status == 429 || status == 402 || status == 432 || status == 433 {
				cooldown = ue.retry
			}
		}
		if status == 0 || status >= 500 {
			cooldown = time.Now().Add(10 * time.Second).Unix()
		}
	}
	a.release(k, status, cooldown, out.Error)
	a.record("key_test", provider.ID, out.OK, int(out.Elapsed), out.Count, out)
	return out, nil
}
