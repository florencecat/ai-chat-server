// Package metrics — метрики Prometheus для ai-server.
//
// Метрики отдаются на отдельном порту (METRICS_ADDR), а не на основном:
// основной открыт в интернет через Caddy, а по /metrics видно маршруты,
// тарифы и частоту ошибок. Отдельный слушатель не публикуется наружу вовсе.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "ai_server"

// Бакеты под долгие ответы: вызов LLM занимает секунды, а не миллисекунды.
var slowBuckets = []float64{.05, .1, .25, .5, 1, 2, 4, 8, 15, 30, 60}

var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "http_requests_total",
		Help:      "HTTP-запросы к ai-server по маршруту и коду ответа.",
	}, []string{"route", "method", "status"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "http_request_duration_seconds",
		Help:      "Время обработки HTTP-запроса.",
		Buckets:   slowBuckets,
	}, []string{"route", "method"})

	httpInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "http_requests_in_flight",
		Help:      "Запросы, обрабатываемые прямо сейчас.",
	})

	llmRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "llm_requests_total",
		Help:      "Вызовы LLM. outcome: ok | truncated | rate_limited | error.",
	}, []string{"provider", "model", "outcome"})

	llmDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "llm_request_duration_seconds",
		Help:      "Длительность одного вызова LLM.",
		Buckets:   slowBuckets,
	}, []string{"provider", "model"})

	llmInvalidAnswers = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "llm_invalid_answers_total",
		Help:      "Ответы модели, не прошедшие разбор JSON.",
	}, []string{"endpoint"})

	chatAnswers = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "chat_answers_total",
		Help:      "Отданные ответы чата. source: cache | llm.",
	}, []string{"endpoint", "tier", "source"})

	chatFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "chat_failures_total",
		Help:      "Неуспешные запросы чата по коду ошибки из ответа.",
	}, []string{"endpoint", "code"})

	redFlagEscalations = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "red_flag_escalations_total",
		Help:      "Ответы, у которых urgency подняли красные флаги.",
	}, []string{"endpoint", "urgency"})

	reports = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reports_total",
		Help:      "Запросы PDF-карточки. result: generated | cached | stale | limited | error.",
	}, []string{"result"})

	reportRender = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "report_render_duration_seconds",
		Help:      "Сборка PDF-карточки без учёта загрузки данных.",
		Buckets:   []float64{.01, .025, .05, .1, .25, .5, 1, 2, 5},
	})

	upstreamRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "upstream_requests_total",
		Help:      "Исходящие HTTP-запросы. status: код ответа или error.",
	}, []string{"service", "method", "status"})

	upstreamDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "upstream_request_duration_seconds",
		Help:      "Длительность исходящего HTTP-запроса.",
		Buckets:   slowBuckets,
	}, []string{"service"})

	webhooks = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rustore_webhooks_total",
		Help:      "Уведомления RuStore. result: applied | ignored | invalid | error.",
	}, []string{"type", "result"})

	buildInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Всегда 1; метки описывают запущенную конфигурацию.",
	}, []string{"llm_provider", "prompt_version"})
)

// SetBuildInfo публикует конфигурацию процесса: на графиках видно, когда
// сменили провайдера или версию промта.
func SetBuildInfo(provider, promptVersion string) {
	buildInfo.WithLabelValues(provider, promptVersion).Set(1)
}

// GinMiddleware считает запросы и их длительность по шаблону маршрута.
// Путь из URL в метку не идёт: /report/vet-card/<id> и пути сканеров дали бы
// неограниченное число рядов. Всё, что не совпало с маршрутом, — "unmatched".
func GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		httpInFlight.Inc()
		defer httpInFlight.Dec()

		c.Next()

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		method := c.Request.Method
		httpRequests.WithLabelValues(route, method, strconv.Itoa(c.Writer.Status())).Inc()
		httpDuration.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
	}
}

// ObserveLLM записывает один вызов модели.
func ObserveLLM(provider, model, outcome string, d time.Duration) {
	if model == "" {
		model = "default"
	}
	llmRequests.WithLabelValues(provider, model, outcome).Inc()
	llmDuration.WithLabelValues(provider, model).Observe(d.Seconds())
}

func LLMInvalidAnswer(endpoint string) { llmInvalidAnswers.WithLabelValues(endpoint).Inc() }

func ChatAnswer(endpoint, tier, source string) {
	chatAnswers.WithLabelValues(endpoint, tier, source).Inc()
}

func ChatFailure(endpoint, code string) { chatFailures.WithLabelValues(endpoint, code).Inc() }

func RedFlagEscalation(endpoint, urgency string) {
	redFlagEscalations.WithLabelValues(endpoint, urgency).Inc()
}

func Report(result string) { reports.WithLabelValues(result).Inc() }

func ReportRender(d time.Duration) { reportRender.Observe(d.Seconds()) }

func Webhook(notificationType, result string) {
	if notificationType == "" {
		notificationType = "unknown"
	}
	webhooks.WithLabelValues(notificationType, result).Inc()
}

// Transport оборачивает HTTP-транспорт клиента внешнего сервиса. nil —
// http.DefaultTransport.
func Transport(service string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripper{service: service, base: base}
}

type roundTripper struct {
	service string
	base    http.RoundTripper
}

func (t roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	status := "error"
	if err == nil {
		status = strconv.Itoa(resp.StatusCode)
	}
	upstreamRequests.WithLabelValues(t.service, req.Method, status).Inc()
	upstreamDuration.WithLabelValues(t.service).Observe(time.Since(start).Seconds())
	return resp, err
}

// Serve поднимает /metrics на addr и гасит его вместе с ctx. Пустой addr —
// метрики выключены.
func Serve(ctx context.Context, addr string) {
	if addr == "" {
		slog.Info("metrics: disabled (METRICS_ADDR is empty)")
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		slog.Info("metrics: listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Без метрик сервис продолжает работать — это не повод падать.
			slog.Error("metrics: server stopped", "err", err)
		}
	}()
}
