// Package rcon 实现 Source RCON 协议客户端，
// 作为优雅停机与运行时设置的备用命令通道。
package rcon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// ErrAuth 表示 RCON 鉴权失败（服务端返回 id=-1）。
var ErrAuth = errors.New("RCON 认证失败：请检查 AdminPassword 与 RCONEnabled")

const (
	typeAuth     = 3
	typeCommand  = 2
	typeResponse = 0
)

// Conn 是一条已通过鉴权的 RCON 连接，非并发安全（同一实例串行 Exec）。
type Conn struct {
	c   net.Conn
	seq int32
}

// Dial 建立 TCP 连接并完成 Source RCON 鉴权；鉴权失败返回 ErrAuth。
func Dial(ctx context.Context, addr, password string) (*Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn := &Conn{c: c}
	if err := conn.write(1, typeAuth, password); err != nil {
		c.Close()
		return nil, err
	}
	// 鉴权读放入 goroutine，主流程 select ctx：服务端 accept 后不回包时
	// 不再永久挂起，超时/取消即关闭连接并报错。
	type authRes struct {
		id  int32
		err error
	}
	ch := make(chan authRes, 1)
	go func() {
		id, _, _, err := conn.read()
		ch <- authRes{id, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			c.Close()
			return nil, r.err
		}
		if r.id == -1 {
			c.Close()
			return nil, ErrAuth
		}
		return conn, nil
	case <-ctx.Done():
		c.Close()
		return nil, fmt.Errorf("RCON 鉴权读超时或被取消: %w", ctx.Err())
	}
}

// write 发送一帧：size = 8+len(body)+2，正文尾部补两个 NUL。
func (c *Conn) write(id, typ int32, body string) error {
	b := make([]byte, 12+len(body)+2)
	binary.LittleEndian.PutUint32(b[0:], uint32(4+4+len(body)+2))
	binary.LittleEndian.PutUint32(b[4:], uint32(id))
	binary.LittleEndian.PutUint32(b[8:], uint32(typ))
	copy(b[12:], body)
	_, err := c.c.Write(b)
	return err
}

// read 读取一帧。size 字段 = 8+len(body)+2，故 id/type 之后的负载为 size-8，
// 已含尾部两个 NUL；返回前去掉这两个 NUL（size>=10 已校验，len(body)>=2 不会越界）。
func (c *Conn) read() (int32, int32, string, error) {
	var size int32
	if err := binary.Read(c.c, binary.LittleEndian, &size); err != nil {
		return 0, 0, "", err
	}
	if size < 10 || size > 8192 {
		return 0, 0, "", fmt.Errorf("RCON 包长度异常: %d", size)
	}
	var id, typ int32
	if err := binary.Read(c.c, binary.LittleEndian, &id); err != nil {
		return 0, 0, "", err
	}
	if err := binary.Read(c.c, binary.LittleEndian, &typ); err != nil {
		return 0, 0, "", err
	}
	body := make([]byte, size-8)
	if _, err := io.ReadFull(c.c, body); err != nil {
		return 0, 0, "", err
	}
	return id, typ, string(body[:len(body)-2]), nil // 去掉尾部 \0\0
}

// Exec 发送命令并等待同 id 的响应包；乱序包（部分服务端先回空包）直接忽略。
func (c *Conn) Exec(cmd string) (string, error) {
	// Conn 非并发安全，串行使用：普通自增即可，无需原子操作
	c.seq++
	id := c.seq
	if err := c.write(id, typeCommand, cmd); err != nil {
		return "", err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.c.SetReadDeadline(deadline)
		rid, typ, body, err := c.read()
		if err != nil {
			return "", err
		}
		if typ == typeResponse && rid == id {
			return body, nil
		}
		// 忽略乱序包（部分服务端先回空包）
	}
}

// Close 关闭底层连接。
func (c *Conn) Close() error { return c.c.Close() }
