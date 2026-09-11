package config

import (
	"log"
	"os"
	"strconv"
	"strings"
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
	// YandexStructured — посылать ли response_format.json_schema. Строгую
	// схему поддерживают не все модели (нужна yandexgpt/rc и новее), поэтому
	// выключатель вынесен в конфигурацию.
	YandexStructured bool

	SystemPrompt string
	// SystemPromptV2 — промт для /v2/chat: подмножество разметки, entries,
	// urgency, местное время пользователя.
	SystemPromptV2 string
	// PromptVersion попадает в ключ кэша и в тело ответа: без него после
	// правки промта пользователи ещё CACHE_TTL получают старые ответы.
	PromptVersion string
	// MarkupEnabled включает блок о разметке в промте v1. По умолчанию
	// выключено: установленные сборки без markdown-рендера показали бы
	// звёздочки текстом. В v2 разметка включена всегда.
	MarkupEnabled bool

	MaxMessageLen int
	// MaxHistoryMessages / MaxHistoryChars ограничивают историю диалога
	// отдельно от лимита на само сообщение пользователя.
	MaxHistoryMessages int
	MaxHistoryChars    int

	// FormatMarkup / FormatPlain подставляются в промт вместо {{FORMAT}}.
	// Держатся файлами, а не константами, чтобы правка разметки не требовала
	// пересборки образа.
	FormatMarkup string
	FormatPlain  string

	// RedFlagsPath — YAML со списком маркеров, поднимающих urgency.
	RedFlagsPath string

	CacheTTL time.Duration
	DBPath   string

	// Квоты бесплатного тарифа. Исторические имена QUOTA_PER_MINUTE /
	// QUOTA_PER_DAY сохранены — премиум задаётся отдельной парой переменных.
	QuotaPerMinute int
	QuotaPerDay    int

	QuotaPerMinutePremium int
	QuotaPerDayPremium    int

	// Модели по тарифам: пустая строка — модель провайдера по умолчанию.
	ModelFree    string
	ModelPremium string

	// EntitlementGrace — сколько ещё действует премиум после expiry.
	// Страховка от опоздавшего вебхука о продлении.
	EntitlementGrace time.Duration

	PBUrl           string
	PBAdminEmail    string
	PBAdminPassword string

	// ── RuStore Public API ────────────────────────────────────────────────
	RuStoreEnabled         bool
	RuStoreAPIURL          string
	RuStoreKeyID           string
	RuStorePrivateKey      string // PEM либо base64(PEM)
	RuStorePackageName     string
	RuStoreAppID           string
	RuStoreSubscriptionIDs []string
	RuStoreSandbox         bool
	RuStoreNotificationKey string
	RuStoreWebhookPath     string
}

func Load() *Config {
	return &Config{
		Port:                 getEnv("PORT", "8080"),
		LLMProvider:          getEnv("LLM_PROVIDER", "yandex"),
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
		YandexMaxTokens:   getEnvInt("YANDEX_MAX_TOKENS", 1500),
		YandexStructured:  getEnvBool("YANDEX_STRUCTURED_OUTPUT", true),

		SystemPrompt:   loadSystemPrompt(),
		SystemPromptV2: loadSystemPromptV2(),
		PromptVersion:  getEnv("PROMPT_VERSION", "v1"),
		MarkupEnabled:  getEnvBool("MARKUP_ENABLED", false),

		MaxMessageLen:      getEnvInt("MAX_MESSAGE_LEN", 4000),
		MaxHistoryMessages: getEnvInt("MAX_HISTORY_MESSAGES", 10),
		MaxHistoryChars:    getEnvInt("MAX_HISTORY_CHARS", 8000),
		FormatMarkup:       loadFile(getEnv("FORMAT_MARKUP_FILE", "prompts/format_markup.txt")),
		FormatPlain:        loadFile(getEnv("FORMAT_PLAIN_FILE", "prompts/format_plain.txt")),
		RedFlagsPath:       getEnv("RED_FLAGS_FILE", "prompts/red-flags.yaml"),

		CacheTTL:       getEnvDuration("CACHE_TTL", "1h"),
		DBPath:         getEnv("DB_PATH", "data/ai-server.db"),
		QuotaPerMinute: getEnvInt("QUOTA_PER_MINUTE", 1),
		QuotaPerDay:    getEnvInt("QUOTA_PER_DAY", 15),

		QuotaPerMinutePremium: getEnvInt("QUOTA_PER_MINUTE_PREMIUM", getEnvInt("QUOTA_PER_MINUTE", 1)),
		QuotaPerDayPremium:    getEnvInt("QUOTA_PER_DAY_PREMIUM", 200),

		ModelFree:    getEnv("MODEL_FREE", ""),
		ModelPremium: getEnv("MODEL_PREMIUM", ""),

		EntitlementGrace: getEnvDuration("ENTITLEMENT_GRACE", "24h"),

		PBUrl:           getEnv("PB_URL", "http://127.0.0.1:8090"),
		PBAdminEmail:    getEnv("PB_ADMIN_EMAIL", ""),
		PBAdminPassword: getEnv("PB_ADMIN_PASSWORD", ""),

		RuStoreEnabled:         getEnvBool("RUSTORE_ENABLED", false),
		RuStoreAPIURL:          strings.TrimRight(getEnv("RUSTORE_API_URL", "https://public-api.rustore.ru"), "/"),
		RuStoreKeyID:           getEnv("RUSTORE_KEY_ID", ""),
		RuStorePrivateKey:      loadRuStorePrivateKey(),
		RuStorePackageName:     getEnv("RUSTORE_PACKAGE_NAME", ""),
		RuStoreAppID:           getEnv("RUSTORE_APP_ID", ""),
		RuStoreSubscriptionIDs: getEnvList("RUSTORE_SUBSCRIPTION_IDS"),
		RuStoreSandbox:         getEnvBool("RUSTORE_SANDBOX", false),
		RuStoreNotificationKey: getEnv("RUSTORE_NOTIFICATION_KEY", ""),
		RuStoreWebhookPath:     getEnv("RUSTORE_WEBHOOK_PATH", "/rustore/webhook"),
	}
}

// loadRuStorePrivateKey возвращает приватный RSA-ключ сервисного доступа.
// Приоритет у файла: многострочный PEM неудобно держать в переменной окружения.
func loadRuStorePrivateKey() string {
	if path := os.Getenv("RUSTORE_PRIVATE_KEY_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		log.Printf("read RUSTORE_PRIVATE_KEY_FILE %q: %v", path, err)
	}
	return getEnv("RUSTORE_PRIVATE_KEY", "")
}

// loadSystemPrompt возвращает системный промт. Приоритет:
// 1) файл из SYSTEM_PROMPT_FILE, 2) переменная SYSTEM_PROMPT, 3) дефолт.
//
// Источник пишется в лог: если в окружении остался старый однострочный
// SYSTEM_PROMPT, он перекроет prompts/system.txt, и правки промта не
// применятся — по логу это видно сразу, иначе пришлось бы гадать.
func loadSystemPrompt() string {
	const fallback = "Ты — полезный ассистент. Отвечай строго в формате JSON. Никакого текста вне JSON-объекта."
	if path := os.Getenv("SYSTEM_PROMPT_FILE"); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			log.Printf("config: system prompt from %s (%d bytes)", path, len(data))
			return string(data)
		} else {
			log.Printf("config: SYSTEM_PROMPT_FILE %q not read: %v", path, err)
		}
	}
	if v := os.Getenv("SYSTEM_PROMPT"); v != "" {
		log.Printf("config: system prompt from SYSTEM_PROMPT env (%d bytes); "+
			"prompts/system.txt is NOT used", len(v))
		return v
	}
	log.Print("config: system prompt is the built-in fallback")
	return fallback
}

// loadSystemPromptV2 возвращает промт для /v2/chat. Приоритет:
// 1) файл из SYSTEM_PROMPT_V2_FILE, 2) prompts/system_v2.txt, 3) промт v1.
// Падать из-за отсутствующего файла нельзя: v1 должен работать в любом случае.
func loadSystemPromptV2() string {
	paths := []string{os.Getenv("SYSTEM_PROMPT_V2_FILE"), "prompts/system_v2.txt"}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if data, err := os.ReadFile(path); err == nil {
			log.Printf("config: v2 system prompt from %s (%d bytes)", path, len(data))
			return string(data)
		}
	}
	log.Print("config: prompts/system_v2.txt not found, /v2/chat falls back to the v1 prompt")
	return loadSystemPrompt()
}

// loadFile читает необязательный кусок промта. Отсутствие файла — не ошибка:
// в промте на его месте окажется пустая строка.
func loadFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("config: optional prompt file %q not loaded: %v", path, err)
		return ""
	}
	return strings.TrimSpace(string(data))
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvList читает список значений через запятую, отбрасывая пустые элементы.
func getEnvList(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
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
