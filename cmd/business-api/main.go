package main

import (
	"av-platform/business/internal/remote"
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"
)

func main() {
	bind := flag.String("listen", "127.0.0.1:8080", "HTTP bind address")
	db := flag.String("db", "remote-control.sqlite", "SQLite path")
	web := flag.String("web", "../livekit-app/web/dist", "Web build directory")
	media := flag.String("media-api", "http://127.0.0.1:7880", "Trusted media API URL")
	public := flag.String("media-url", "ws://127.0.0.1:7880", "Client-visible media URL")
	cert := flag.String("tls-cert", "", "Optional HTTPS certificate")
	key := flag.String("tls-key", "", "Optional HTTPS private key")
	flag.Parse()
	apiKey, secret := os.Getenv("LIVEKIT_API_KEY"), os.Getenv("LIVEKIT_API_SECRET")
	if apiKey == "" || len(secret) < 24 {
		log.Fatal("Set LIVEKIT_API_KEY and LIVEKIT_API_SECRET (at least 24 characters)")
	}
	store, err := remote.OpenSQLite(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	svc, err := remote.New(ctx, store, &remote.LiveKit{URL: *media, PublicURL: *public, Key: apiKey, Secret: secret, Client: &http.Client{Timeout: 2 * time.Second}})
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", svc.Handler())
	mux.Handle("/healthz", svc.Handler())
	mux.Handle("/", http.FileServer(http.Dir(*web)))
	server := &http.Server{Addr: *bind, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				svc.Sweep(ctx)
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("business API listening on %s", *bind)
	if *cert != "" {
		err = server.ListenAndServeTLS(*cert, *key)
	} else {
		err = server.ListenAndServe()
	}
	if err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
