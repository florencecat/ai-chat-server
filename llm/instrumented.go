package llm

import (
	"errors"
	"time"

	"ai-server/metrics"
)

// Instrument оборачивает провайдера сбором метрик: число вызовов по исходу и
// их длительность. Повторы внутри handlers считаются отдельными вызовами —
// это то, за что платим провайдеру.
func Instrument(p Provider) Provider { return instrumented{p} }

type instrumented struct{ Provider }

func (i instrumented) Chat(req Request) (Response, error) {
	start := time.Now()
	resp, err := i.Provider.Chat(req)

	outcome := "ok"
	switch {
	case errors.Is(err, ErrTooManyRequests):
		outcome = "rate_limited"
	case err != nil:
		outcome = "error"
	case resp.Truncated():
		outcome = "truncated"
	}
	metrics.ObserveLLM(i.Name(), req.Model, outcome, time.Since(start))
	return resp, err
}
