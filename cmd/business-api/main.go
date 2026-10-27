package main

import (
	"av-platform/business/internal/bootstrap"
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
)

func main() {
	var cfg bootstrap.Config
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "HTTP bind address")
	flag.StringVar(&cfg.Database, "db", "remote-control.sqlite", "SQLite path")
	flag.StringVar(&cfg.RemoteControlWeb, "remote-control-web", "../livekit-app/apps/remote-control/web/dist", "Remote-control Web build directory; empty disables serving")
	flag.StringVar(&cfg.ScreenCastWeb, "screen-cast-web", "../livekit-app/apps/screen-cast/web/dist", "Screen-cast Web build directory; empty disables serving")
	flag.StringVar(&cfg.MediaAPI, "media-api", "http://127.0.0.1:7880", "Trusted media API URL")
	flag.StringVar(&cfg.MediaURL, "media-url", "ws://127.0.0.1:7880", "Client-visible media URL")
	flag.StringVar(&cfg.TLSCert, "tls-cert", "", "Optional HTTPS certificate")
	flag.StringVar(&cfg.TLSKey, "tls-key", "", "Optional HTTPS private key")
	flag.Parse()
	cfg.APIKey, cfg.APISecret = os.Getenv("LIVEKIT_API_KEY"), os.Getenv("LIVEKIT_API_SECRET")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := bootstrap.Run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}
