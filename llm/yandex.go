package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"ai-server/config"
)

// yandexProvider — клиент Yandex AI Studio (эндпоинт /v1/responses).
type yandexProvider struct {
	cfg        *config.Config
	httpClient *http.Client
}

func newYandex(cfg *config.Config) *yandexProvider {
	return &yandexProvider{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *yandexProvider) Name() string { return "yandex" }

type yandexRequest struct {
	Model           string  `json:"model"`
	Temperature     float64 `json:"temperature"`
	Instructions    string  `json:"instructions"`
	Input           string  `json:"input"`
	MaxOutputTokens int     `json:"max_output_tokens"`
}

type yandexResponse struct {
	Output []struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

func (r yandexResponse) text() string {
	for _, o := range r.Output {
		if len(o.Content) > 0 {
			return o.Content[0].Text
		}
	}
	return ""
}

func (p *yandexProvider) Chat(systemPrompt, userInput string) (string, error) {
	reqData := yandexRequest{
		Model:           fmt.Sprintf("gpt://%s/%s", p.cfg.YandexFolderID, p.cfg.YandexModel),
		Temperature:     p.cfg.YandexTemperature,
		Instructions:    systemPrompt,
		Input:           userInput,
		MaxOutputTokens: p.cfg.YandexMaxTokens,
	}
	body, err := json.Marshal(reqData)
	if err != nil {
		return "", fmt.Errorf("yandex marshal: %w", err)
	}

	req, err := http.NewRequest("POST",
		"https://ai.api.cloud.yandex.net/v1/responses",
		bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("yandex request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Api-Key "+p.cfg.YandexAPIKey)
	req.Header.Set("OpenAI-Project", p.cfg.YandexFolderID)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("yandex do: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("yandex read: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", ErrTooManyRequests
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("yandex failed %d: %s", resp.StatusCode, respBody)
	}

	var yResp yandexResponse
	if err := json.Unmarshal(respBody, &yResp); err != nil {
		return "", fmt.Errorf("yandex parse: %w", err)
	}
	return yResp.text(), nil
}
