package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestGinMiddlewareUsesRouteTemplate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/report/vet-card/:pet_id", func(c *gin.Context) { c.Status(http.StatusOK) })

	for _, path := range []string{"/report/vet-card/a1", "/report/vet-card/b2", "/.env"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	if got := testutil.ToFloat64(httpRequests.WithLabelValues("/report/vet-card/:pet_id", "GET", "200")); got != 2 {
		t.Errorf("route counter = %v, want 2", got)
	}
	if got := testutil.ToFloat64(httpRequests.WithLabelValues("unmatched", "GET", "404")); got != 1 {
		t.Errorf("unmatched counter = %v, want 1", got)
	}
}

func TestTransportCountsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	client := &http.Client{Transport: Transport("test", nil)}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Неотвечающий адрес: запрос падает до получения кода.
	if resp, err := client.Get("http://127.0.0.1:1"); err == nil {
		resp.Body.Close()
	}

	if got := testutil.ToFloat64(upstreamRequests.WithLabelValues("test", "GET", "502")); got != 1 {
		t.Errorf("502 counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(upstreamRequests.WithLabelValues("test", "GET", "error")); got != 1 {
		t.Errorf("error counter = %v, want 1", got)
	}
}
