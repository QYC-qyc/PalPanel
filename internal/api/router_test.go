package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"palpanel/internal/config"
)

func TestHealthz(t *testing.T) {
	r := New(Deps{Cfg: config.Config{Listen: ":0", DataDir: t.TempDir()}})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 got %d", w.Code)
	}
}
