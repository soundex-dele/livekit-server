// Package bootstrap is the composition root for independently owned businesses.
package bootstrap

import (
	"av-platform/business/internal/businesses/remotecontrol/application"
	remoteinfra "av-platform/business/internal/businesses/remotecontrol/infrastructure"
	remotehttp "av-platform/business/internal/businesses/remotecontrol/transport/http"
	castapp "av-platform/business/internal/businesses/screencast/application"
	castinfra "av-platform/business/internal/businesses/screencast/infrastructure"
	casthttp "av-platform/business/internal/businesses/screencast/transport/http"
	httpx "av-platform/business/internal/infrastructure/http"
	"av-platform/business/internal/platform/media"
	"context"
	"fmt"
	"log"
	"net/http"
	"time"
)

type Config struct {
	Listen, Database, RemoteControlWeb, ScreenCastWeb, MediaAPI, MediaURL string
	APIKey, APISecret, TLSCert, TLSKey                                    string
}

// Handler registers each business explicitly. Unknown APIs never fall through to static files.
type Product struct {
	Handler http.Handler
	Web     string
}

func Handler(remote http.Handler, web string, cast ...Product) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(remotehttp.Prefix, remote)
	if len(cast) > 0 {
		mux.Handle(casthttp.Prefix, cast[0].Handler)
		if cast[0].Web != "" {
			mux.Handle("/apps/screen-cast/", http.StripPrefix("/apps/screen-cast/", http.FileServer(http.Dir(cast[0].Web))))
		}
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { httpx.Fail(w, 404, "not_found") })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { httpx.Reply(w, 200, map[string]bool{"ok": true}) })
	if web != "" {
		mux.Handle("/apps/remote-control/", http.StripPrefix("/apps/remote-control/", http.FileServer(http.Dir(web))))
	}
	return mux
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.APIKey == "" || len(cfg.APISecret) < 24 {
		return fmt.Errorf("set LIVEKIT_API_KEY and LIVEKIT_API_SECRET (at least 24 characters)")
	}
	store, err := remoteinfra.OpenSQLite(cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	svc, err := application.New(ctx, store, &remoteinfra.Media{Client: &media.LiveKit{
		URL: cfg.MediaAPI, PublicURL: cfg.MediaURL, Key: cfg.APIKey, Secret: cfg.APISecret,
		Client: &http.Client{Timeout: 2 * time.Second},
	}}, nil)
	if err != nil {
		return err
	}
	castStore, err := castinfra.OpenSQLite(cfg.Database)
	if err != nil {
		return err
	}
	defer castStore.Close()
	castSvc, err := castapp.New(ctx, castStore, &castinfra.Media{Client: &media.LiveKit{URL: cfg.MediaAPI, PublicURL: cfg.MediaURL, Key: cfg.APIKey, Secret: cfg.APISecret, Client: &http.Client{Timeout: 2 * time.Second}}}, nil)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: cfg.Listen, Handler: Handler(remotehttp.New(svc), cfg.RemoteControlWeb, Product{casthttp.New(castSvc), cfg.ScreenCastWeb}),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				svc.Sweep(runCtx)
				castSvc.Sweep(runCtx)
			}
		}
	}()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-runCtx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("business API listening on %s", cfg.Listen)
	if cfg.TLSCert != "" {
		err = server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = server.ListenAndServe()
	}
	cancel()
	<-swept
	<-shutdownDone
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
