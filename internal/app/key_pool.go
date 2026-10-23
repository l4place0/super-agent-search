package app

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// keyPool owns per-process scheduling state. SQL configuration/material stays
// in store.go; network adapters never hold poolMu or update credential rows.
// App composes these boundaries for one process, not distributed leases.
type keyPool struct {
	poolMu    sync.Mutex
	flight    map[int64]int
	turns     map[int64]float64
	poolFault atomic.Bool
}

// A lease is shared by Key copies and consumed under poolMu exactly once.
// Occupancy is per ID across revisions; credential state is revision-scoped.
type keyLease struct{ released bool }

var errNoKey = errors.New("no available key (missing, disabled, cooling down or at concurrency limit)")

func (a *App) acquire(provider string, exclude map[int64]bool) (Key, error) {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	if a.poolFault.Load() {
		return Key{}, errStorage
	}
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
	err = a.recordKeyUse(k)
	if err != nil {
		a.flight[k.ID]--
		return Key{}, err
	}
	k.lease = &keyLease{}
	return k, nil
}
func (a *App) release(k Key, status int, cooldown int64, reason string) error {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	if k.lease == nil || k.lease.released {
		return nil
	}
	k.lease.released = true
	a.flight[k.ID]--
	if a.flight[k.ID] == 0 {
		delete(a.flight, k.ID)
	}
	err := a.applyKeyOutcome(k, status, cooldown, reason)
	if err != nil {
		a.poolFault.Store(true)
	} // Unknown persisted state: fail closed until restart.
	return err
}
