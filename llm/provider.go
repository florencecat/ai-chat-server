// Package llm абстрагирует LLM-провайдера, чтобы смена агента (GigaChat,
// Yandex и т.д.) сводилась к переменной окружения LLM_PROVIDER.
package llm

import (
	"errors"
	"fmt"

	"ai-server/config"
)

// ErrTooManyRequests — единый признак rate-limit со стороны провайдера.
var ErrTooManyRequests = errors.New("llm: too many requests")

// Provider — единый интерфейс к LLM. Принимает имя модели, системный промт и
// сообщение пользователя, возвращает текст ответа модели.
//
// model выбирается по тарифу пользователя (базовая/улучшенная); пустая строка
// означает модель провайдера по умолчанию из конфигурации.
type Provider interface {
	Chat(model, systemPrompt, userInput string) (string, error)
	Name() string
}

// New выбирает и валидирует провайдера по cfg.LLMProvider.
func New(cfg *config.Config) (Provider, error) {
	switch cfg.LLMProvider {
	case "yandex":
		if cfg.YandexFolderID == "" || cfg.YandexAPIKey == "" {
			return nil, errors.New("YANDEX_FOLDER_ID and YANDEX_API_KEY must be set")
		}
		return newYandex(cfg), nil
	case "gigachat", "":
		if cfg.GigaChatAuthKey == "" && (cfg.GigaChatClientID == "" || cfg.GigaChatClientSecret == "") {
			return nil, errors.New("set GIGACHAT_AUTH_KEY or both GIGACHAT_CLIENT_ID and GIGACHAT_CLIENT_SECRET")
		}
		return newGigaChat(cfg), nil
	default:
		return nil, fmt.Errorf("unknown LLM_PROVIDER: %q", cfg.LLMProvider)
	}
}
