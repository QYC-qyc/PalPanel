package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"palpanel/internal/audit"
	"palpanel/internal/auth"
	"palpanel/internal/config"
	"palpanel/internal/db"
)

func newTestRouter(t *testing.T) (*gin.Engine, *auth.Service) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	if err := auth.Seed(d); err != nil {
		t.Fatal(err)
	}
	secret, err := auth.LoadOrCreateSecret(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.New(d, secret)
	return New(Deps{
		Cfg:    config.Config{Listen: ":0", DataDir: dir},
		DB:     d,
		Auth:   svc,
		Audit:  audit.New(d),
		Secret: secret,
	}), svc
}

func getJSON(r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return m
}

func postJSON(r *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}
