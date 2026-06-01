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
	"ai-server/gigachat"
	"ai-server/handlers"
	"ai-server/pocketbase"
)

func main() {
	_ = godotenv.Load()
	cfg := config.Load()

	if cfg.GigaChatClientID == "" || cfg.GigaChatClientSecret == "" {
		log.Fatal("GIGACHAT_CLIENT_ID and GIGACHAT_CLIENT_SECRET must be set")
	}
	if cfg.PBAdminEmail == "" || cfg.PBAdminPassword == "" {
		log.Fatal("PB_ADMIN_EMAIL and PB_ADMIN_PASSWORD must be set")
	}

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
	gcClient := gigachat.NewClient(cfg)
	h := handlers.New(gcClient, cacheStore, pbClient, cfg)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	r.GET("/health", h.Health)
	r.POST("/chat", h.Chat)
	r.GET("/quota/:token", h.GetQuota)

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
