package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"uni-agent-search/internal/app"
)

// The gate prevents late handlers from touching App once cleanup has begun.
// WaitGroup.Add and closing are serialized, so Wait cannot race a new Add.
type requestGate struct {
	mu      sync.Mutex
	closing bool
	active  sync.WaitGroup
	next    http.Handler
}

func (g *requestGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	g.active.Add(1)
	g.mu.Unlock()
	defer g.active.Done()
	g.next.ServeHTTP(w, r)
}
func (g *requestGate) finish() <-chan struct{} {
	g.mu.Lock()
	g.closing = true
	g.mu.Unlock()
	done := make(chan struct{})
	go func() { g.active.Wait(); close(done) }()
	return done
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	a, err := app.New(os.Getenv("SAS_DATA_DIR"))
	if err != nil {
		return err
	}
	// If a handler cannot be cancelled, exit without racing its database access.
	// The OS reclaims resources on this exceptional, bounded failure path.
	safeToClose := true
	defer func() {
		if safeToClose {
			if err := a.Close(); err != nil {
				log.Printf("close application: %v", err)
			}
		}
	}()
	addr := os.Getenv("SAS_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	gate := &requestGate{next: a.Handler()}
	s := &http.Server{Addr: addr, Handler: gate, BaseContext: func(net.Listener) context.Context { return requests }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 100 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- s.Serve(listener) }()
	log.Printf("Uni Agent Search %s listening on %s; credentials: %s/credentials.json", app.Version, listener.Addr(), a.DataDir)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-served:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = s.Shutdown(shutdown)
	cancel()
	if err != nil {
		log.Print("shutdown drain timed out; cancelling active requests")
		cancelRequests()
		_ = s.Close()
	}
	// Even Close does not wait for handlers. Fence late entries and join those
	// already admitted before releasing transports or SQLite.
	done := gate.finish()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		safeToClose = false
		return errors.New("shutdown: handlers did not stop after cancellation; forcing process exit")
	}
	cancelRequests()
	if serveErr == nil {
		serveErr = <-served
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}
	return nil
}
