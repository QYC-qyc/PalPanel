package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newServer(t *testing.T, wantPath, wantMethod string, status int, resp string, gotAuth *string, gotBody *string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath || r.Method != wantMethod {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		u, p, ok := r.BasicAuth()
		*gotAuth = u + ":" + p
		if !ok || u != "admin" || p != "pw" {
			t.Fatalf("auth %q:%q ok=%v", u, p, ok)
		}
		if r.Body != nil {
			buf := make([]byte, 256)
			n, _ := r.Body.Read(buf)
			*gotBody = string(buf[:n])
		}
		w.WriteHeader(status)
		w.Write([]byte(resp))
	}))
}

func newClient(s *httptest.Server) *Client {
	// Client 需支持注入 baseURL+跳过证书校验：New 内部解析 host:port；测试用 NewForTest(rawURL, pass) 直连
	return NewForTest(s.URL, "pw")
}

func TestInfo(t *testing.T) {
	var auth, body string
	s := newServer(t, "/v1/api/info", "GET", 200,
		`{"servername":"srv","version":"v0.1.0","description":"d"}`, &auth, &body)
	defer s.Close()
	info, err := newClient(s).Info(context.Background())
	if err != nil || info.ServerName != "srv" || info.Version != "v0.1.0" {
		t.Fatalf("%+v %v", info, err)
	}
	if auth != "admin:pw" {
		t.Fatalf("auth %q", auth)
	}
}

func TestPlayersBareArray(t *testing.T) {
	var auth, body string
	s := newServer(t, "/v1/api/players", "GET", 200,
		`[{"name":"a","uid":"1","steamid":"s1","ping":12.5}]`, &auth, &body)
	defer s.Close()
	ps, err := newClient(s).Players(context.Background())
	if err != nil || len(ps) != 1 || ps[0].UID != "1" || ps[0].Ping != 12.5 {
		t.Fatalf("%+v %v", ps, err)
	}
}

func TestKickBody(t *testing.T) {
	var auth, body string
	s := newServer(t, "/v1/api/kick", "POST", 200, ``, &auth, &body)
	defer s.Close()
	if err := newClient(s).Kick(context.Background(), "42"); err != nil {
		t.Fatal(err)
	}
	if body != `{"userid":"42"}` {
		t.Fatalf("body %q", body)
	}
}

func TestAuthError(t *testing.T) {
	var auth, body string
	s := newServer(t, "/v1/api/info", "GET", 401, `{"error":"x"}`, &auth, &body)
	defer s.Close()
	if _, err := newClient(s).Info(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth got %v", err)
	}
}

func TestAnnounceBody(t *testing.T) {
	var auth, body string
	s := newServer(t, "/v1/api/announce", "POST", 200, ``, &auth, &body)
	defer s.Close()
	if err := newClient(s).Announce(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if body != `{"message":"hello"}` {
		t.Fatalf("body %q", body)
	}
}
