package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"palpanel/internal/event"
)

func TestWSEvents(t *testing.T) {
	r, deps := newTestDeps(t)
	srv := httptest.NewServer(r)
	defer srv.Close()
	postJSON(r, "/api/v1/setup", "", map[string]string{"username": "root", "password": "good-pass-1"})
	token := loginToken(t, r, "root", "good-pass-1")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws?token=" + token

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	deps.Hub.Broadcast(event.Event{Type: "instance.status", InstanceID: 1, Payload: "running"})
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var e event.Event
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	if e.Type != "instance.status" || e.Payload != "running" {
		t.Fatalf("event %+v", e)
	}
}

func TestWSRejectsBadToken(t *testing.T) {
	r, _ := newTestDeps(t)
	srv := httptest.NewServer(r)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/ws?token=bad", nil)
	if err == nil {
		t.Fatal("bad token should be rejected")
	}
}
