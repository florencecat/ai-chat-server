package llm

import (
	"errors"
	"fmt"

	"ai-server/config"
	"ai-server/gigachat"
)

// gigaChatProvider оборачивает существующий gigachat.Client под интерфейс
// Provider. Развитие остановлено: сервер нацелен на Yandex, здесь поддержана
// только базовая отправка диалога — строгой схемы ответа нет, формат держится
// на промте и на серверной нормализации.
type gigaChatProvider struct {
	client *gigachat.Client
}

func newGigaChat(cfg *config.Config) *gigaChatProvider {
	return &gigaChatProvider{client: gigachat.NewClient(cfg)}
}

func (p *gigaChatProvider) Name() string { return "gigachat" }

func (p *gigaChatProvider) SupportsSchema() bool { return false }

func (p *gigaChatProvider) Chat(req Request) (string, error) {
	messages := make([]gigachat.Message, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		messages = append(messages, gigachat.Message{Role: "system", Content: req.SystemPrompt})
	}
	for _, m := range req.Messages {
		messages = append(messages, gigachat.Message{Role: m.Role, Content: m.Content})
	}

	resp, err := p.client.Chat(req.Model, messages)
	if err != nil {
		if errors.Is(err, gigachat.ErrTooManyRequests) {
			return "", ErrTooManyRequests
		}
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("gigachat: empty choices")
	}
	return resp.Choices[0].Message.Content, nil
}
