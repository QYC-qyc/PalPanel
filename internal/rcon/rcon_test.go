package rcon

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func startFakeServer(t *testing.T, password string, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				// 读一帧：size 字段 = 8+len(body)+2，id/type 之后的负载为 size-8，已含尾部两个 NUL
				read := func() (int32, int32, string, bool) {
					var size, id, typ int32
					if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
						return 0, 0, "", false
					}
					if err := binary.Read(r, binary.LittleEndian, &id); err != nil {
						return 0, 0, "", false
					}
					if err := binary.Read(r, binary.LittleEndian, &typ); err != nil {
						return 0, 0, "", false
					}
					if size < 10 {
						return 0, 0, "", false
					}
					body := make([]byte, size-8)
					if _, err := io.ReadFull(r, body); err != nil {
						return 0, 0, "", false
					}
					return id, typ, string(body[:len(body)-2]), true // 去掉尾部 \0\0
				}
				id, typ, body, ok := read()
				if !ok {
					return
				}
				if typ != 3 || body != password {
					write(c, -1, 2, "")
					return
				}
				write(c, id, 2, "")
				cid, ctyp, _, ok := read()
				if !ok || ctyp != 2 {
					return
				}
				write(c, cid, 0, reply)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func write(c net.Conn, id, typ int32, body string) {
	b := make([]byte, 4+4+4+len(body)+2)
	binary.LittleEndian.PutUint32(b[0:], uint32(4+4+len(body)+2))
	binary.LittleEndian.PutUint32(b[4:], uint32(id))
	binary.LittleEndian.PutUint32(b[8:], uint32(typ))
	copy(b[12:], body)
	c.Write(b)
}

func TestAuthAndExec(t *testing.T) {
	addr := startFakeServer(t, "secret", "OK: ShowPlayers")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Dial(ctx, addr, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := c.Exec("ShowPlayers")
	if err != nil || out != "OK: ShowPlayers" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestAuthFailure(t *testing.T) {
	addr := startFakeServer(t, "secret", "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := Dial(ctx, addr, "wrong"); !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth got %v", err)
	}
}
