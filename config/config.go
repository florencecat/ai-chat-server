package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port string

	LLMProvider string // "gigachat" | "yandex"

	GigaChatAuthURL      string
	GigaChatBaseURL      string
	GigaChatClientID     string
	GigaChatClientSecret string
	GigaChatAuthKey      string // готовая Base64-строка «Авторизационные данные» из ЛК
	GigaChatScope        string
	GigaChatModel        string
	GigaChatSkipTLS      bool

	YandexFolderID    string
	YandexAPIKey      string
	YandexModel       string
	YandexTemperature float64
	YandexMaxTokens   int

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
		LLMProvider:          getEnv("LLM_PROVIDER", "gigachat"),
		GigaChatAuthURL:      getEnv("GIGACHAT_AUTH_URL", "https://ngw.devices.sberbank.ru:9443/api/v2/oauth"),
		GigaChatBaseURL:      getEnv("GIGACHAT_BASE_URL", "https://gigachat.devices.sberbank.ru/api/v1"),
		GigaChatClientID:     getEnv("GIGACHAT_CLIENT_ID", ""),
		GigaChatClientSecret: getEnv("GIGACHAT_CLIENT_SECRET", ""),
		GigaChatAuthKey:      getEnv("GIGACHAT_AUTH_KEY", ""),
		GigaChatScope:        getEnv("GIGACHAT_SCOPE", "GIGACHAT_API_PERS"),
		GigaChatModel:        getEnv("GIGACHAT_MODEL", "GigaChat"),
		GigaChatSkipTLS:      getEnvBool("GIGACHAT_SKIP_TLS", true),

		YandexFolderID:    getEnv("YANDEX_FOLDER_ID", ""),
		YandexAPIKey:      getEnv("YANDEX_API_KEY", ""),
		YandexModel:       getEnv("YANDEX_MODEL", "yandexgpt-lite/latest"),
		YandexTemperature: getEnvFloat("YANDEX_TEMPERATURE", 0.25),
		YandexMaxTokens:   getEnvInt("YANDEX_MAX_TOKENS", 500),

		SystemPrompt: loadSystemPrompt(),
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

// loadSystemPrompt возвращает системный промт. Приоритет:
// 1) файл из SYSTEM_PROMPT_FILE, 2) переменная SYSTEM_PROMPT, 3) дефолт.
func loadSystemPrompt() string {
	const fallback = "Ты — полезный ассистент. Отвечай строго в формате JSON. Никакого текста вне JSON-объекта."
	if path := os.Getenv("SYSTEM_PROMPT_FILE"); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			return string(data)
		}
	}
	return getEnv("SYSTEM_PROMPT", fallback)
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

func getEnvFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
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
