package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRoutedOnlyLoggerSkipsUnknownPaths(t *testing.T) {
	var buf bytes.Buffer
	prev := gin.DefaultWriter
	gin.DefaultWriter = &buf
	defer func() { gin.DefaultWriter = prev }()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(routedOnlyLogger())
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })

	for _, path := range []string{"/.env", "/phpinfo.php", "/health"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	out := buf.String()
	if !strings.Contains(out, "/health") {
		t.Errorf("known route not logged: %q", out)
	}
	if strings.Contains(out, ".env") || strings.Contains(out, "phpinfo") {
		t.Errorf("unknown paths logged: %q", out)
	}
}

func TestTrustedProxiesIgnoreSpoofedHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if err := r.SetTrustedProxies([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	var got string
	r.GET("/ip", func(c *gin.Context) { got = c.ClientIP() })

	cases := []struct{ remote, want string }{
		{"127.0.0.1:5000", "1.2.3.4"},             // Caddy: заголовку верим
		{"34.116.116.158:5000", "34.116.116.158"}, // чужой адрес: заголовок игнорируем
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/ip", nil)
		req.RemoteAddr = tc.remote
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		r.ServeHTTP(httptest.NewRecorder(), req)
		if got != tc.want {
			t.Errorf("remote %s: ClientIP = %s, want %s", tc.remote, got, tc.want)
		}
	}
}
