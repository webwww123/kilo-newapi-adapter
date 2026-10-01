package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/webwww123/kilo-newapi-adapter/internal/adapter"
)

func main() {
	cfg := adapter.ConfigFromEnv()
	addr := strings.TrimSpace(os.Getenv("ADDR"))
	if addr == "" {
		addr = ":18083"
	}

	handler, cleanup := adapter.NewHandlerWithCleanup(cfg)
	defer cleanup()
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Printf("kilo-newapi-adapter listening on %s, upstream=%s", addr, cfg.UpstreamBaseURL)
		errs <- server.ListenAndServe()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-signals:
		log.Printf("received signal %s, shutting down", sig)
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown failed: %v\n", err)
		os.Exit(1)
	}
}
