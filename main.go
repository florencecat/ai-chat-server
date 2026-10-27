package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	// tzdata вшивается в бинарник: образ на alpine без пакета tzdata, а
	// time.LoadLocation нужен, чтобы понимать «Europe/Moscow» от клиента.
	_ "time/tzdata"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	bolt "go.etcd.io/bbolt"

	"ai-server/advice"
	"ai-server/cache"
	"ai-server/config"
	"ai-server/entitlement"
	"ai-server/handlers"
	"ai-server/llm"
	"ai-server/metrics"
	"ai-server/pocketbase"
	"ai-server/rustore"
)

func main() {
	_ = godotenv.Load()
	setupLogger(os.Getenv("LOG_FORMAT"), os.Getenv("LOG_LEVEL"))
	cfg := config.Load()

	if cfg.PBAdminEmail == "" || cfg.PBAdminPassword == "" {
		fatal("PB_ADMIN_EMAIL and PB_ADMIN_PASSWORD must be set")
	}

	llmProvider, err := llm.New(cfg)
	if err != nil {
		fatal("init llm provider", "err", err)
	}
	slog.Info("llm provider", "name", llmProvider.Name(), "structured_output", llmProvider.SupportsSchema())
	metrics.SetBuildInfo(llmProvider.Name(), cfg.PromptVersion)
	llmProvider = llm.Instrument(llmProvider)

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o750); err != nil {
		fatal("create data dir", "err", err)
	}

	db, err := bolt.Open(cfg.DBPath, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		fatal("open db", "err", err)
	}
	defer db.Close()

	cacheStore, err := cache.New(db, cfg.CacheTTL)
	if err != nil {
		fatal("init cache", "err", err)
	}

	pbClient := pocketbase.NewClient(cfg)

	// RuStore-интеграция необязательна: без неё сервер работает, все
	// пользователи остаются на бесплатном тарифе, а /verify отвечает 503.
	ruClient, err := rustore.NewClient(cfg)
	if err != nil {
		slog.Error("rustore: verification disabled, init failed", "err", err)
		ruClient = nil
	} else if ruClient == nil {
		slog.Info("rustore: verification disabled (RUSTORE_ENABLED is not set)")
	} else {
		slog.Info("rustore: enabled", "sandbox", cfg.RuStoreSandbox, "products", ruClient.SubscriptionIDs())
	}

	plans := entitlement.Plans{
		Free: entitlement.Plan{
			Tier:           entitlement.TierFree,
			Model:          cfg.ModelFree,
			QuotaPerDay:    cfg.QuotaPerDay,
			QuotaPerMinute: cfg.QuotaPerMinute,
		},
		Premium: entitlement.Plan{
			Tier:           entitlement.TierPremium,
			Model:          cfg.ModelPremium,
			QuotaPerDay:    cfg.QuotaPerDayPremium,
			QuotaPerMinute: cfg.QuotaPerMinutePremium,
		},
	}
	entService := entitlement.NewService(pbClient, ruClient, plans, cfg.EntitlementGrace)

	var notificationKey []byte
	if cfg.RuStoreNotificationKey != "" {
		notificationKey, err = rustore.ParseNotificationKey(cfg.RuStoreNotificationKey)
		if err != nil {
			fatal("rustore notification key", "err", err)
		}
	} else {
		slog.Info("rustore: webhook disabled (RUSTORE_NOTIFICATION_KEY is not set)")
	}

	// Красные флаги — страховка от заниженного urgency. Без файла сервер
	// работает, просто без эскалации, поэтому это не фатальная ошибка.
	redFlags, err := advice.LoadRedFlags(cfg.RedFlagsPath)
	if err != nil {
		slog.Warn("red flags: disabled", "err", err)
		redFlags = &advice.RedFlags{}
	}
	slog.Info("red flags loaded", "markers", redFlags.Len(), "path", cfg.RedFlagsPath)

	h := handlers.New(llmProvider, cacheStore, pbClient, cfg, entService, redFlags, notificationKey)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		fatal("TRUSTED_PROXIES", "err", err)
	}
	r.Use(metrics.GinMiddleware(), routedOnlyLogger(), gin.Recovery())

	r.GET("/health", h.Health)
	r.POST("/chat", h.Chat)
	r.POST("/v2/chat", h.ChatV2)
	r.GET("/quota", h.GetQuota)
	r.GET("/entitlement", h.GetEntitlement)
	r.POST("/verify", h.VerifyPurchase)
	r.GET("/report/vet-card/:pet_id", h.VetCard)
	r.POST(cfg.RuStoreWebhookPath, h.RuStoreWebhook)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		slog.Info("server listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("server error", "err", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	metrics.Serve(ctx, cfg.MetricsAddr)
	<-ctx.Done()

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}

// setupLogger включает JSON-логи в stdout: их собирает Alloy и разбирает Loki
// по полям level, msg, endpoint и т.д. LOG_FORMAT=text — для локальной
// отладки. Стандартный log после SetDefault тоже идёт через этот обработчик.
func setupLogger(format, level string) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

// routedOnlyLogger пишет access-лог только для зарегистрированных маршрутов.
// Сканеры перебирают сотни путей вроде /.env и /phpinfo.php, и эти 404
// забивают лог. Их всё равно видно в access-логе Caddy, по которому работает
// fail2ban, и в метрике с route="unmatched".
func routedOnlyLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		// FullPath пуст, когда запрос не совпал ни с одним маршрутом.
		route := c.FullPath()
		if route == "" {
			return
		}
		status := c.Writer.Status()
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}
		slog.Log(c.Request.Context(), level, "http request",
			"method", c.Request.Method,
			"route", route,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
			"bytes", c.Writer.Size(),
		)
	}
}
