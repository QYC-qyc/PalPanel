package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"palpanel/internal/auth"
)

// handleWS 处理 GET /api/ws?token=<JWT>：校验 token 与会话后，把 Hub 事件
// 以文本帧 JSON 推送给客户端。不走 gin 鉴权中间件（浏览器 WebSocket 无法带
// Authorization 头），鉴权在 handler 内完成。
func (d Deps) handleWS(c *gin.Context) {
	token := c.Query("token")
	uid, rv, err := auth.ParseToken(d.Auth.Secret, token)
	if err != nil {
		fail(c, http.StatusUnauthorized, "登录已失效")
		return
	}
	if _, err := d.Auth.SessionUser(uid, rv); err != nil {
		fail(c, http.StatusUnauthorized, "登录已失效，请重新登录")
		return
	}
	conn, err := websocket.Accept(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ch, cancel := d.Hub.Subscribe()
	defer cancel()

	ctx := c.Request.Context()
	done := make(chan struct{})
	go func() { // 读侧：客户端断开时退出；消息内容忽略
		defer close(done)
		for {
			if _, _, err := conn.Reader(ctx); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		case e := <-ch:
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			err = conn.Write(wctx, websocket.MessageText, b)
			wcancel()
			if err != nil { // 写失败（含慢消费者超时）：断开，由 Hub 丢弃机制兜底
				return
			}
		}
	}
}
