package app

import (
	"context"
	"errors"
	"time"
)

// A manual test targets exactly the selected key, including disabled/cooling
// keys. It never enables the key or changes provider configuration implicitly.
func (a *App) testKey(ctx context.Context, id int64) (Attempt, error) {
	all, e := a.keys("")
	if e != nil {
		return Attempt{}, e
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
		return Attempt{}, errors.New("key not found")
	}
	ps, e := a.providers()
	if e != nil {
		return Attempt{}, e
	}
	var provider Provider
	for _, p := range ps {
		if p.ID == k.ProviderID {
			provider = p
			break
		}
	}
	if provider.ID == "" {
		return Attempt{}, errors.New("provider not found")
	}
	k.secret, e = a.decrypt(k.secret)
	if e != nil {
		return Attempt{}, errors.New("key decryption failed")
	}
	a.poolMu.Lock()
	if a.flight[id] >= k.MaxConcurrent {
		a.poolMu.Unlock()
		return Attempt{}, errors.New("key is at its concurrency limit")
	}
	a.flight[id]++
	_, e = a.db.Exec(`UPDATE api_keys SET uses=uses+1 WHERE id=?`, id)
	if e != nil {
		a.flight[id]--
		a.poolMu.Unlock()
		return Attempt{}, e
	}
	a.poolMu.Unlock()
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
