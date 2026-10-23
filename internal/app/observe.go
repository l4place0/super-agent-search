package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"
)

var errStorage = errors.New("database unavailable")

type requestMetrics struct {
	Requests         atomic.Int64
	Successes        atomic.Int64
	CacheHits        atomic.Int64
	Rejected         atomic.Int64
	UpstreamFailures atomic.Int64
}
type observationKey struct{}
type observation struct {
	id       string
	query    string
	count    int
	details  any
	cacheHit bool
}

func (a *App) storageError(operation string, err error) error {
	if err == nil {
		return nil
	}
	// Never log the driver's text: SQL errors may contain bound data.
	a.logger.Error("storage failure", "operation", operation, "error_kind", "database")
	return errStorage
}
func (a *App) operationRecord(ctx context.Context, operation, query string, ok bool, elapsed, count int, details any) {
	if o, yes := ctx.Value(observationKey{}).(*observation); yes {
		o.query, o.count, o.details = query, count, details
		return
	}
	a.record(operation, query, ok, elapsed, count, details)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Recover logs only frame locations, never panic values, arguments, headers,
// URLs or response bodies. Request IDs are generated locally, not trusted input.
func (a *App) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		o := &observation{id: token()[:16]}
		w.Header().Set("X-Request-ID", o.id)
		sw := &statusWriter{ResponseWriter: w}
		operation := ""
		if r.Method == "POST" && r.URL.Path == "/v1/search" {
			operation = "search"
		}
		if r.Method == "POST" && r.URL.Path == "/v1/fetch" {
			operation = "fetch"
		}
		r = r.WithContext(context.WithValue(r.Context(), observationKey{}, o))
		defer func() {
			panicked := false
			if recover() != nil {
				panicked = true
				pcs := make([]uintptr, 24)
				n := runtime.Callers(3, pcs)
				frames := runtime.CallersFrames(pcs[:n])
				locations := []string{}
				for {
					f, more := frames.Next()
					locations = append(locations, fmt.Sprintf("%s:%d %s", filepath.Base(f.File), f.Line, f.Function))
					if !more {
						break
					}
				}
				a.logger.Error("request panic", "request_id", o.id, "frames", locations)
				if sw.status == 0 {
					fail(sw, 500, "internal server error")
				}
			}
			status := sw.status
			if status == 0 {
				status = 200
			}
			if panicked {
				status = 500
			}
			if status >= 500 {
				a.logger.Error("request failed", "request_id", o.id, "status", status)
			}
			if operation != "" {
				a.metrics.Requests.Add(1)
				if status >= 200 && status < 300 {
					a.metrics.Successes.Add(1)
				}
				if status == 429 {
					a.metrics.Rejected.Add(1)
				}
				if o.cacheHit {
					a.metrics.CacheHits.Add(1)
				}
				a.record(operation, o.query, status >= 200 && status < 300, int(time.Since(start).Milliseconds()), o.count, map[string]any{"request_id": o.id, "status": status, "cache_hit": o.cacheHit, "result": o.details})
				a.logger.Info("request completed", "request_id", o.id, "operation", operation, "status", status, "cache_hit", o.cacheHit, "elapsed_ms", time.Since(start).Milliseconds())
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

func (a *App) ready(ctx context.Context) bool {
	if a.poolFault.Load() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var providers, keys, requests int
	err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM providers),EXISTS(SELECT 1 FROM api_keys),EXISTS(SELECT 1 FROM requests)`).Scan(&providers, &keys, &requests)
	return a.storageError("readiness", err) == nil
}
