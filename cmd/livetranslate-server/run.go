package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"livetranslate/server/db"
	"livetranslate/server/internal/admin"
	"livetranslate/server/internal/audit"
	"livetranslate/server/internal/auth"
	"livetranslate/server/internal/config"
	"livetranslate/server/internal/httpapi"
	accountapi "livetranslate/server/internal/httpapi/accountapi"
	attachmentapi "livetranslate/server/internal/httpapi/attachmentapi"
	authapi "livetranslate/server/internal/httpapi/auth"
	"livetranslate/server/internal/httpapi/middleware"
	syncapi "livetranslate/server/internal/httpapi/syncapi"
	"livetranslate/server/internal/mail"
	"livetranslate/server/internal/metrics"
	"livetranslate/server/internal/storage"
	"livetranslate/server/internal/store"
	syncpkg "livetranslate/server/internal/sync"
	"livetranslate/server/internal/token"
	"livetranslate/server/internal/webapp"
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

// setupLogging configures the structured logger level before any component
// constructs its slog.Default()-derived logger.
func setupLogging(cfg *config.Config) {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}

// runServe is the /v1 API: token auth (Apple + email/password), sync
// protocol, account routes. The admin UI is a separate process/listener.
func runServe() error {
	cfg := loadOrExit()
	setupLogging(cfg)
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

	// Attachment file storage (optional — enabled by ATTACHMENT_STORAGE_DIR).
	// Metadata sync works without it; when nil, attachment files stay
	// local-only on the clients.
	var attachmentStore *storage.Store
	if cfg.AttachmentStorageDir != "" {
		var err error
		attachmentStore, err = storage.NewStore(cfg.AttachmentStorageDir)
		if err != nil {
			return fmt.Errorf("attachment storage: %w", err)
		}
		syncSvc.SetAttachmentStore(attachmentStore)
		slog.Info("attachment storage enabled", "dir", cfg.AttachmentStorageDir)
	}

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
	if attachmentStore != nil {
		attachmentH := attachmentapi.NewHandler(st, authH, attachmentStore)
		attachmentH.SetMaxUploadBytes(cfg.AttachmentMaxUploadBytes)
		attachmentH.Register(mux)
	}
	accountH := accountapi.NewHandler(st, authH, authSvc)
	if attachmentStore != nil {
		accountH.SetAttachmentStore(attachmentStore)
	}
	accountH.Register(mux)

	// Public web surfaces: reset deep-link landing + AASA.
	webapp.NewHandler(cfg, st, authSvc, trust).Register(mux)

	// Internal metrics (aggregate counters only). Gate at the reverse proxy
	// — the endpoint itself has no auth by design (Prometheus scrape).
	if cfg.MetricsEnabled {
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = w.Write([]byte(metrics.Default().Render()))
		})
	}
	// pprof: default OFF. When enabled, answers loopback clients only.
	if cfg.PprofEnabled {
		mux.HandleFunc("/debug/pprof/", pprofGate(loopbackOnly(pprofIndex)))
		mux.HandleFunc("/debug/pprof/cmdline", pprofGate(loopbackOnly(pprofCmdline)))
		mux.HandleFunc("/debug/pprof/profile", pprofGate(loopbackOnly(pprofProfile)))
		mux.HandleFunc("/debug/pprof/symbol", pprofGate(loopbackOnly(pprofSymbol)))
		mux.HandleFunc("/debug/pprof/trace", pprofGate(loopbackOnly(pprofTrace)))
	}

	var root http.Handler = mux
	root = countingMiddleware(root)
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

	if cfg.AASATeamID == "" {
		slog.Warn("AASA_TEAM_ID is not configured — /.well-known/apple-app-site-association serves an empty app list (Universal Links will not open the app until a real Team ID is set)")
	}

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
	setupLogging(cfg)
	st := connectOrExit(cfg)
	defer st.Close()

	auditor := audit.NewRecorder(st)
	mailer := mail.NewSender(cfg)
	authSvc := auth.NewService(cfg, st, token.NewManager(cfg), mailer, auditor)
	svc := admin.NewService(cfg, st, auditor, authSvc)
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

// countingMiddleware feeds the /metrics request counters (path + status
// only — no bodies, no auth headers, no query strings).
func countingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &countWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		metrics.Inc(metrics.HTTPRequestsTotal)
		if sw.status >= 500 {
			metrics.Inc(metrics.HTTP5xxTotal)
		}
	})
}

type countWriter struct {
	http.ResponseWriter
	status int
}

func (w *countWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
