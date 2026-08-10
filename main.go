package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	bolt "go.etcd.io/bbolt"

	"ai-server/cache"
	"ai-server/config"
	"ai-server/entitlement"
	"ai-server/handlers"
	"ai-server/llm"
	"ai-server/pocketbase"
	"ai-server/rustore"
)

func main() {
	_ = godotenv.Load()
	cfg := config.Load()

	if cfg.PBAdminEmail == "" || cfg.PBAdminPassword == "" {
		log.Fatal("PB_ADMIN_EMAIL and PB_ADMIN_PASSWORD must be set")
	}

	llmProvider, err := llm.New(cfg)
	if err != nil {
		log.Fatalf("init llm provider: %v", err)
	}
	log.Printf("llm provider: %s", llmProvider.Name())

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o750); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	db, err := bolt.Open(cfg.DBPath, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cacheStore, err := cache.New(db, cfg.CacheTTL)
	if err != nil {
		log.Fatalf("init cache: %v", err)
	}

	pbClient := pocketbase.NewClient(cfg)

	// RuStore-интеграция необязательна: без неё сервер работает, все
	// пользователи остаются на бесплатном тарифе, а /verify отвечает 503.
	ruClient, err := rustore.NewClient(cfg)
	if err != nil {
		log.Fatalf("init rustore client: %v", err)
	}
	if ruClient == nil {
		log.Print("rustore: verification disabled (RUSTORE_ENABLED is not set)")
	} else {
		log.Printf("rustore: enabled (sandbox=%v, products=%v)", cfg.RuStoreSandbox, ruClient.SubscriptionIDs())
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
			log.Fatalf("rustore notification key: %v", err)
		}
	} else {
		log.Print("rustore: webhook disabled (RUSTORE_NOTIFICATION_KEY is not set)")
	}

	h := handlers.New(llmProvider, cacheStore, pbClient, cfg, entService, notificationKey)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	r.GET("/health", h.Health)
	r.POST("/chat", h.Chat)
	r.GET("/quota", h.GetQuota)
	r.GET("/entitlement", h.GetEntitlement)
	r.POST("/verify", h.VerifyPurchase)
	r.POST(cfg.RuStoreWebhookPath, h.RuStoreWebhook)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("server listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
