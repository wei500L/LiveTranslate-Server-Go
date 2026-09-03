package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"livetranslate/server/db"
	"livetranslate/server/internal/admin"
	"livetranslate/server/internal/audit"
	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/config"
	"livetranslate/server/internal/httpapi"
	accountapi "livetranslate/server/internal/httpapi/accountapi"
	authapi "livetranslate/server/internal/httpapi/auth"
	"livetranslate/server/internal/httpapi/middleware"
	syncapi "livetranslate/server/internal/httpapi/syncapi"
	"livetranslate/server/internal/mail"
	"livetranslate/server/internal/store"
	syncpkg "livetranslate/server/internal/sync"
	"livetranslate/server/internal/token"
)

func loadOrExit() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	return cfg
}

func connectOrExit(cfg *config.Config) *store.DB {
	st, err := store.NewDB(context.Background(), cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres:", err)
		os.Exit(1)
	}
	return st
}

func runMigrate() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return db.Migrate(cfg.DatabaseURL)
}

// runServe is the /v1 API: token auth (Apple + email/password), sync
// protocol, account routes. The admin UI is a separate process/listener.
func runServe() error {
	cfg := loadOrExit()
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	st := connectOrExit(cfg)
	defer st.Close()

	tokens := token.NewManager(cfg)
	mailer := mail.NewSender(cfg)
	auditor := audit.NewRecorder(st)
	authSvc := auth.NewService(cfg, st, tokens, mailer, auditor)
	syncSvc := syncpkg.NewService(cfg, st)

	trust, err := middleware.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return fmt.Errorf("trusted proxies: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		if err := st.Pool.Ping(r.Context()); err != nil {
			httpapi.WriteDetail(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	authH := authapi.NewHandler(cfg, authSvc, tokens, st, trust)
	authH.Register(mux)
	syncapi.NewHandler(cfg, syncSvc, authH).Register(mux)
	accountapi.NewHandler(st, authH, authSvc).Register(mux)

	var root http.Handler = mux
	root = httpapi.CORS(cfg.CORSOrigins, root)
	root = httpapi.Handler(root.(httpapi.Router), cfg.MaxBodyBytes, 30*time.Second)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	syncSvc.StartCleanupLoop(ctx, 24*time.Hour)

	go func() {
		fmt.Println("api listening on", cfg.ListenAddr, "(dev_login:", cfg.DevLoginEnabled, ")")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "serve:", err)
			stop()
		}
	}()

	<-ctx.Done()
	fmt.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// runAdmin is the operations UI on its own listener, with its own cookie
// session stack. It never serves /v1 traffic.
func runAdmin() error {
	cfg := loadOrExit()
	st := connectOrExit(cfg)
	defer st.Close()

	auditor := audit.NewRecorder(st)
	svc := admin.NewService(cfg, st, auditor)
	h := admin.NewHandler(cfg, svc)

	mux := http.NewServeMux()
	h.Register(mux)
	root := httpapi.Handler(mux, 1<<20, 15*time.Second)

	srv := &http.Server{
		Addr:              cfg.AdminListenAddr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		fmt.Println("admin listening on", cfg.AdminListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "admin serve:", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
