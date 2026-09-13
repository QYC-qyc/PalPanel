package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func newServer(t *testing.T, wantPath, wantMethod string, status int, resp string, gotAuth *string, gotBody *string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath || r.Method != wantMethod {
			t.Errorf("%s %s", r.Method, r.URL.Path)
			return
		}
		u, p, ok := r.BasicAuth()
		*gotAuth = u + ":" + p
		if !ok || u != "admin" || p != "pw" {
			t.Errorf("auth %q:%q ok=%v", u, p, ok)
			return
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

// TestPlayersWrappedObject：包裹形态 {"players":[...]}，首次解码失败后二次请求成功。
func TestPlayersWrappedObject(t *testing.T) {
	var auth, body string
	s := newServer(t, "/v1/api/players", "GET", 200,
		`{"players":[{"name":"w","uid":"2","steamid":"s2","ping":3.5}]}`, &auth, &body)
	defer s.Close()
	ps, err := newClient(s).Players(context.Background())
	if err != nil || len(ps) != 1 || ps[0].UID != "2" || ps[0].Ping != 3.5 {
		t.Fatalf("%+v %v", ps, err)
	}
}

// TestPlayersNonShapeErrorNoRetry：非形状错误（401）不做二次请求，直接返回 ErrAuth。
func TestPlayersNonShapeErrorNoRetry(t *testing.T) {
	var hits int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"x"}`))
	}))
	defer s.Close()
	if _, err := newClient(s).Players(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth got %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("非形状错误不应二次请求：hits = %d", n)
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
