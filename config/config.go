package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port string

	GigaChatAuthURL      string
	GigaChatBaseURL      string
	GigaChatClientID     string
	GigaChatClientSecret string
	GigaChatAuthKey      string // готовая Base64-строка «Авторизационные данные» из ЛК
	GigaChatScope        string
	GigaChatModel        string
	GigaChatSkipTLS      bool

	SystemPrompt  string
	MaxMessageLen int

	CacheTTL time.Duration
	DBPath   string

	QuotaPerMinute int
	QuotaPerDay    int

	PBUrl           string
	PBAdminEmail    string
	PBAdminPassword string
}

func Load() *Config {
	return &Config{
		Port:                 getEnv("PORT", "8080"),
		GigaChatAuthURL:      getEnv("GIGACHAT_AUTH_URL", "https://ngw.devices.sberbank.ru:9443/api/v2/oauth"),
		GigaChatBaseURL:      getEnv("GIGACHAT_BASE_URL", "https://gigachat.devices.sberbank.ru/api/v1"),
		GigaChatClientID:     getEnv("GIGACHAT_CLIENT_ID", ""),
		GigaChatClientSecret: getEnv("GIGACHAT_CLIENT_SECRET", ""),
		GigaChatAuthKey:      getEnv("GIGACHAT_AUTH_KEY", ""),
		GigaChatScope:        getEnv("GIGACHAT_SCOPE", "GIGACHAT_API_PERS"),
		GigaChatModel:        getEnv("GIGACHAT_MODEL", "GigaChat"),
		GigaChatSkipTLS:      getEnvBool("GIGACHAT_SKIP_TLS", true),
		SystemPrompt: getEnv("SYSTEM_PROMPT",
			"Ты — полезный ассистент. Отвечай строго в формате JSON. Никакого текста вне JSON-объекта."),
		MaxMessageLen:  getEnvInt("MAX_MESSAGE_LEN", 4000),
		CacheTTL:       getEnvDuration("CACHE_TTL", "1h"),
		DBPath:         getEnv("DB_PATH", "data/ai-server.db"),
		QuotaPerMinute: getEnvInt("QUOTA_PER_MINUTE", 1),
		QuotaPerDay:    getEnvInt("QUOTA_PER_DAY", 15),

		PBUrl:           getEnv("PB_URL", "http://127.0.0.1:8090"),
		PBAdminEmail:    getEnv("PB_ADMIN_EMAIL", ""),
		PBAdminPassword: getEnv("PB_ADMIN_PASSWORD", ""),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func getEnvDuration(key, fallback string) time.Duration {
	s := getEnv(key, fallback)
	d, err := time.ParseDuration(s)
	if err != nil {
		d, _ = time.ParseDuration(fallback)
	}
	return d
}
