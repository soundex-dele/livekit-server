package main

import (
	"av-platform/business/internal/bootstrap"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"platform-foundation/server/telemetry"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if e := run(); e != nil {
		slog.Error("business service stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command == "openapi" {
		raw, e := bootstrap.AVOpenAPI()
		if e == nil {
			_, e = os.Stdout.Write(append(raw, '\n'))
		}
		return e
	}
	c, e := bootstrap.LoadAVConfig()
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "migrate" {
		return bootstrap.MigrateAV(ctx, c)
	}
	stop, e := telemetry.Start(ctx)
	if e != nil {
		return e
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stop(ctx)
	}()
	a, e := bootstrap.NewAV(c, nil)
	if e != nil {
		return e
	}
	defer a.Close()
	switch command {
	case "api":
		return a.Serve(ctx, false)
	case "serve":
		return a.Serve(ctx, true)
	case "worker":
		return a.Foundation.Workers(ctx)
	case "bootstrap":
		return a.Bootstrap(ctx, os.Getenv("PF_ADMIN_NAME"), os.Getenv("PF_ADMIN_PASSWORD"), os.Getenv("AV_ORGANIZATION_NAME"))
	default:
		return fmt.Errorf("unknown command %q; use migrate, bootstrap, api, worker, serve, openapi", command)
	}
}
