package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var ErrAuth = errors.New("REST 认证失败：请检查 AdminPassword 与 RESTAPIEnabled")

type Info struct {
	ServerName  string `json:"servername"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type Player struct {
	Name    string  `json:"name"`
	UID     string  `json:"uid"`
	SteamID string  `json:"steamid"`
	Ping    float64 `json:"ping"`
}

type Client struct {
	base string
	pass string
	hc   *http.Client
}

// New：生产用，host:gamePort
func New(host string, port int, adminPassword string) *Client {
	return NewForTest(fmt.Sprintf("https://%s:%d", host, port), adminPassword)
}

// NewForTest：base 直填（测试注入 httptest 地址）
func NewForTest(base, adminPassword string) *Client {
	return &Client{base: strings.TrimRight(base, "/") + "/v1/api", pass: adminPassword,
		hc: &http.Client{Timeout: 15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth("admin", c.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrAuth
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("REST %d: %s", resp.StatusCode, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Probe(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/info", nil, nil)
}
func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.do(ctx, http.MethodGet, "/info", nil, &i)
	return i, err
}
func (c *Client) Metrics(ctx context.Context) (map[string]any, error) {
	var m map[string]any
	err := c.do(ctx, http.MethodGet, "/metrics", nil, &m)
	return m, err
}

func (c *Client) Players(ctx context.Context) ([]Player, error) {
	var arr []Player
	err := c.do(ctx, http.MethodGet, "/players", nil, &arr)
	if err == nil {
		return arr, nil
	}
	var wrap struct {
		Players []Player `json:"players"`
	}
	if err2 := c.do(ctx, http.MethodGet, "/players", nil, &wrap); err2 == nil {
		return wrap.Players, nil
	}
	return nil, err
}

func (c *Client) Kick(ctx context.Context, uid string) error {
	return c.do(ctx, http.MethodPost, "/kick", map[string]string{"userid": uid}, nil)
}
func (c *Client) Ban(ctx context.Context, uid string) error {
	return c.do(ctx, http.MethodPost, "/ban", map[string]string{"userid": uid}, nil)
}
func (c *Client) Unban(ctx context.Context, uid, steamID string) error {
	return c.do(ctx, http.MethodPost, "/unban", map[string]string{"userid": uid, "steamid": steamID}, nil)
}
func (c *Client) Announce(ctx context.Context, msg string) error {
	return c.do(ctx, http.MethodPost, "/announce", map[string]string{"message": msg}, nil)
}
func (c *Client) Save(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/save", map[string]any{}, nil)
}
func (c *Client) Shutdown(ctx context.Context, waitSec int, msg string) error {
	return c.do(ctx, http.MethodPost, "/shutdown", map[string]any{"waittime": waitSec, "message": msg}, nil)
}
func (c *Client) Stop(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/stop", map[string]any{}, nil)
}
