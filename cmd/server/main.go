package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"uni-agent-search/internal/app"
)

func main() {
	a, err := app.New(os.Getenv("SAS_DATA_DIR"))
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()
	addr := os.Getenv("SAS_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	s := &http.Server{Addr: addr, Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 100 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdown)
	}()
	log.Printf("Uni Agent Search %s listening on %s; credentials: %s/credentials.json", app.Version, addr, a.DataDir)
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
