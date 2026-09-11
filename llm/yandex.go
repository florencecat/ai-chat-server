package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"ai-server/config"
)

// yandexProvider — клиент Yandex AI Studio, OpenAI-совместимый эндпоинт
// /v1/chat/completions. Он выбран вместо /v1/responses по двум причинам:
// принимает историю диалога массивом сообщений и поддерживает строгую схему
// ответа через response_format.json_schema.
type yandexProvider struct {
	cfg        *config.Config
	httpClient *http.Client
}

const yandexChatURL = "https://ai.api.cloud.yandex.net/v1/chat/completions"

func newYandex(cfg *config.Config) *yandexProvider {
	return &yandexProvider{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *yandexProvider) Name() string { return "yandex" }

func (p *yandexProvider) SupportsSchema() bool { return p.cfg.YandexStructured }

type yandexRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
	Stream         bool            `json:"stream"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
}

type yandexResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
}

// modelURI приводит имя модели к виду gpt://<folder>/<model>. Готовый URI
// (уже с gpt://) пропускается как есть — так в MODEL_PREMIUM можно указать
// модель из чужого каталога.
func (p *yandexProvider) modelURI(model string) string {
	if model == "" {
		model = p.cfg.YandexModel
	}
	if strings.HasPrefix(model, "gpt://") {
		return model
	}
	return fmt.Sprintf("gpt://%s/%s", p.cfg.YandexFolderID, model)
}

func (p *yandexProvider) Chat(req Request) (Response, error) {
	out, status, body, err := p.do(req, req.JSONSchema)
	if err != nil {
		return Response{}, err
	}

	// Строгую схему поддерживают не все модели каталога. Вместо того чтобы
	// уронить чат целиком, повторяем запрос без схемы: формат ответа тогда
	// держится на промте, а лишнее всё равно срежет нормализация.
	if status == http.StatusBadRequest && req.JSONSchema != nil && mentionsSchema(body) {
		log.Printf("yandex: model %s rejected response_format, retrying without schema: %s",
			req.Model, truncate(body, 300))
		out, status, body, err = p.do(req, nil)
		if err != nil {
			return Response{}, err
		}
	}

	switch {
	case status == http.StatusTooManyRequests:
		return Response{}, ErrTooManyRequests
	case status != http.StatusOK:
		return Response{}, fmt.Errorf("yandex failed %d: %s", status, truncate(body, 500))
	}
	return out, nil
}

// do выполняет один запрос. Ошибку возвращает только на уровне транспорта:
// неуспешный HTTP-статус отдаётся вызывающему вместе с телом, чтобы тот мог
// решить, повторять ли запрос.
func (p *yandexProvider) do(req Request, schema json.RawMessage) (out Response, status int, body []byte, err error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.cfg.YandexMaxTokens
	}

	messages := make([]Message, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		messages = append(messages, Message{Role: "system", Content: req.SystemPrompt})
	}
	messages = append(messages, req.Messages...)

	reqData := yandexRequest{
		Model:       p.modelURI(req.Model),
		Messages:    messages,
		Temperature: p.cfg.YandexTemperature,
		MaxTokens:   maxTokens,
		Stream:      false,
	}
	if schema != nil {
		name := req.SchemaName
		if name == "" {
			name = "answer"
		}
		reqData.ResponseFormat = &responseFormat{
			Type:       "json_schema",
			JSONSchema: jsonSchema{Name: name, Schema: schema},
		}
	}

	payload, err := json.Marshal(reqData)
	if err != nil {
		return out, 0, nil, fmt.Errorf("yandex marshal: %w", err)
	}

	httpReq, err := http.NewRequest("POST", yandexChatURL, bytes.NewReader(payload))
	if err != nil {
		return out, 0, nil, fmt.Errorf("yandex request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Api-Key "+p.cfg.YandexAPIKey)
	httpReq.Header.Set("OpenAI-Project", p.cfg.YandexFolderID)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return out, 0, nil, fmt.Errorf("yandex do: %w", err)
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return out, resp.StatusCode, nil, fmt.Errorf("yandex read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, resp.StatusCode, body, nil
	}

	var yResp yandexResponse
	if err := json.Unmarshal(body, &yResp); err != nil {
		return out, resp.StatusCode, body, fmt.Errorf("yandex parse: %w", err)
	}
	if len(yResp.Choices) == 0 {
		return out, resp.StatusCode, body, fmt.Errorf("yandex: empty choices")
	}
	choice := yResp.Choices[0]
	return Response{
		Content:      choice.Message.Content,
		FinishReason: choice.FinishReason,
	}, resp.StatusCode, body, nil
}

// mentionsSchema отличает «схему не приняли» от прочих 400.
//
// Ищем просто «schema»: Yandex пишет и «json_schema», и «Invalid JSON Schema»
// через пробел, и чинить этот список по одному поводу за раз — значит каждый
// раз ронять чат вместо деградации в ответ без схемы.
func mentionsSchema(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "schema") ||
		strings.Contains(s, "response_format") ||
		strings.Contains(s, "structured")
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
