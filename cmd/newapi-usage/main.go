package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mucsbr/newapi-usage/internal/audit"
	"github.com/mucsbr/newapi-usage/internal/channels"
	"github.com/mucsbr/newapi-usage/internal/config"
	"github.com/mucsbr/newapi-usage/internal/server"
	"github.com/mucsbr/newapi-usage/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg)
	if err != nil {
		slog.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	var auditIndex *audit.Indexer
	stopAudit := func() {}
	if cfg.AuditLogGlob != "" {
		aud, err := audit.Open(audit.Config{
			LogGlob:         cfg.AuditLogGlob,
			IndexDSN:        cfg.AuditIndexDSN,
			TimeZone:        cfg.AuditTimezone,
			ScanInterval:    cfg.AuditScanInterval,
			LookupWindow:    cfg.AuditLookupWindow,
			MaxLinesPerScan: cfg.AuditMaxLinesPerScan,
		}, func(key string) (audit.ResolvedToken, error) {
			token, err := st.ResolveTokenByKey(key)
			if err != nil {
				return audit.ResolvedToken{}, err
			}
			return audit.ResolvedToken{
				TokenID: token.TokenID,
				Name:    token.Name,
				KeyTail: token.KeyTail,
			}, nil
		})
		if err != nil {
			slog.Error("audit index failed", "error", err)
			os.Exit(1)
		}
		auditIndex = aud
		auditCtx, cancelAudit := context.WithCancel(context.Background())
		stopAudit = cancelAudit
		auditIndex.Start(auditCtx)
		defer func() {
			stopAudit()
			if err := auditIndex.Close(); err != nil {
				slog.Error("audit index close failed", "error", err)
			}
		}()
	}

	chManager := channels.New(cfg)
	stopChannels := func() {}
	if chManager.Enabled() {
		chCtx, cancelChannels := context.WithCancel(context.Background())
		stopChannels = cancelChannels
		chManager.Start(chCtx)
		defer func() {
			stopChannels()
			chManager.Close()
		}()
	}

	app := server.New(st, auditIndex, chManager, cfg.AdminPassword)
	httpServer := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		slog.Info("newapi-usage listening",
			"addr", cfg.Addr(),
			"driver", cfg.DBDriver,
			"show_full_keys", cfg.ShowFullKeys,
			"audit_enabled", auditIndex != nil && auditIndex.Enabled(),
			"audit_glob", cfg.AuditLogGlob,
			"audit_timezone", cfg.AuditTimezone,
			"channels_enabled", chManager.Enabled(),
		)
		if err := httpServer.ListenAndServe(); err != nil && !server.IsServerClosed(err) {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	stopAudit()
	stopChannels()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("shutdown failed", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}
