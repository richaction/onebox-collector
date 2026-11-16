package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/richaction/onebox-collector/internal/channel/wecomkf"
	"github.com/richaction/onebox-collector/internal/config"
	"github.com/richaction/onebox-collector/internal/core"
	"github.com/richaction/onebox-collector/internal/resolver"
	"github.com/richaction/onebox-collector/internal/store"
	"github.com/richaction/onebox-collector/internal/vault"
	"github.com/richaction/onebox-collector/internal/webdav"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("collector stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	callback, err := wecomkf.NewCallback(cfg.Corp, cfg.Token, cfg.AESKey)
	if err != nil {
		return err
	}
	writer := vault.New(cfg.Vault, cfg.MediaLimit)
	if err := writer.Init(func(path string) { log.Warn("unexpected Vault entry", "path", path) }); err != nil {
		return err
	}
	db, err := store.Open(cfg.Data)
	if err != nil {
		return err
	}
	defer db.Close()
	httpClient := resolver.PublicHTTP(12 * time.Second)
	service := &core.Service{
		DB: db, Vault: writer, Log: log,
		KF:     &wecomkf.Client{Base: wecomkf.APIBase, Corp: cfg.Corp, Secret: cfg.Secret, HTTP: noRedirect(12 * time.Second)},
		Douyin: &resolver.Douyin{HTTP: httpClient, Assets: writer, Cache: db, MaxDuration: cfg.DouyinDuration},
		Official: &resolver.Official{HTTP: httpClient, Assets: writer, History: db, Enabled: cfg.OfficialFetch,
			Timeout: cfg.OfficialTimeout, MaxImages: cfg.OfficialImages, UA: cfg.OfficialUA},
	}
	if cfg.DAV != nil {
		service.Sync = &webdav.Client{Base: cfg.DAV, User: cfg.DAVUser, Password: cfg.DAVPassword, HTTP: noRedirect(15 * time.Second), Vault: writer}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var workers sync.WaitGroup
	workers.Go(func() { service.RunPuller(ctx) })
	workers.Go(func() { service.RunWriter(ctx) })
	workers.Go(func() { service.RunMaintenance(ctx) })
	if service.Sync != nil {
		workers.Go(func() { service.RunSyncer(ctx) })
	}
	public := server(cfg.Listen, service.Public(cfg.Callback, callback))
	internal := server(cfg.InternalListen, service.Internal(cfg.ReplayToken))
	errs := make(chan error, 2)
	for _, httpServer := range []*http.Server{public, internal} {
		go func() { errs <- httpServer.ListenAndServe() }()
	}
	log.Info("collector started", "callback_path", cfg.Callback, "listen", cfg.Listen, "internal", cfg.InternalListen, "webdav", cfg.DAV != nil)
	select {
	case <-ctx.Done():
	case err = <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = errors.Join(err, public.Shutdown(shutdown), internal.Shutdown(shutdown))
	workers.Wait()
	return err
}

func server(addr string, handler http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 45 * time.Second}
}

func noRedirect(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
