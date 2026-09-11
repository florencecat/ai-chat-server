// Package llm абстрагирует LLM-провайдера, чтобы смена агента (Yandex,
// GigaChat и т.д.) сводилась к переменной окружения LLM_PROVIDER.
package llm

import (
	"encoding/json"
	"errors"
	"fmt"

	"ai-server/config"
)

// ErrTooManyRequests — единый признак rate-limit со стороны провайдера.
var ErrTooManyRequests = errors.New("llm: too many requests")

// Message — одно сообщение диалога. Роли: system | user | assistant.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request — запрос к модели.
//
// Model выбирается по тарифу пользователя (базовая/улучшенная); пустая строка
// означает модель провайдера по умолчанию из конфигурации.
type Request struct {
	Model        string
	SystemPrompt string
	// Messages — история диалога без системного сообщения; последний элемент
	// это текущий вопрос пользователя.
	Messages []Message
	// MaxTokens ограничивает длину ответа; 0 — значение из конфигурации.
	MaxTokens int
	// JSONSchema — строгая схема ответа. nil означает «без схемы»: тогда
	// формат задаётся только системным промтом.
	JSONSchema json.RawMessage
	// SchemaName требуется провайдерам как имя схемы в запросе.
	SchemaName string
}

// Response — ответ модели вместе с тем, почему генерация закончилась.
//
// FinishReason нужен вызывающему: "length" означает, что ответ оборвался на
// полуслове и JSON в нём заведомо неполный. Без этого признака обрыв по
// лимиту токенов неотличим от пустого ответа — оба дают «unexpected end of
// JSON input», но лечатся по-разному.
type Response struct {
	Content string
	// FinishReason: "stop" | "length" | "" (провайдер не сообщил).
	FinishReason string
}

// Truncated сообщает, что модель упёрлась в потолок max_tokens.
func (r Response) Truncated() bool { return r.FinishReason == "length" }

// Provider — единый интерфейс к LLM.
type Provider interface {
	Chat(req Request) (Response, error)
	Name() string
	// SupportsSchema сообщает, умеет ли провайдер строгую схему ответа.
	// Если нет, вызывающему остаётся полагаться на промт и нормализацию.
	SupportsSchema() bool
}

// New выбирает и валидирует провайдера по cfg.LLMProvider.
func New(cfg *config.Config) (Provider, error) {
	switch cfg.LLMProvider {
	case "yandex", "":
		if cfg.YandexFolderID == "" || cfg.YandexAPIKey == "" {
			return nil, errors.New("YANDEX_FOLDER_ID and YANDEX_API_KEY must be set")
		}
		return newYandex(cfg), nil
	case "gigachat":
		if cfg.GigaChatAuthKey == "" && (cfg.GigaChatClientID == "" || cfg.GigaChatClientSecret == "") {
			return nil, errors.New("set GIGACHAT_AUTH_KEY or both GIGACHAT_CLIENT_ID and GIGACHAT_CLIENT_SECRET")
		}
		return newGigaChat(cfg), nil
	default:
		return nil, fmt.Errorf("unknown LLM_PROVIDER: %q", cfg.LLMProvider)
	}
}
