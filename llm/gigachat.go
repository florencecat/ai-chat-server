package llm

import (
	"errors"
	"fmt"

	"ai-server/config"
	"ai-server/gigachat"
)

// gigaChatProvider оборачивает существующий gigachat.Client под интерфейс Provider.
type gigaChatProvider struct {
	client *gigachat.Client
}

func newGigaChat(cfg *config.Config) *gigaChatProvider {
	return &gigaChatProvider{client: gigachat.NewClient(cfg)}
}

func (p *gigaChatProvider) Name() string { return "gigachat" }

func (p *gigaChatProvider) Chat(systemPrompt, userInput string) (string, error) {
	messages := []gigachat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userInput},
	}
	resp, err := p.client.Chat(messages)
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
