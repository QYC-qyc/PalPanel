package event

import (
	"testing"
	"time"
)

func TestSubscribeBroadcast(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	defer cancel()
	h.Broadcast(Event{Type: "test", InstanceID: 7, Payload: "hello"})
	select {
	case e := <-ch:
		if e.Type != "test" || e.InstanceID != 7 {
			t.Fatalf("got %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

func TestCancelStopsDelivery(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	cancel()
	h.Broadcast(Event{Type: "after-cancel"})
	select {
	case <-ch:
		t.Fatal("cancelled subscriber should not receive")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	h := NewHub()
	ch, _ := h.Subscribe() // 不消费
	for i := 0; i < 300; i++ {
		h.Broadcast(Event{Type: "flood"}) // 超过 256 缓冲，必须不阻塞不 panic
	}
	_ = ch
}
