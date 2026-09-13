package event

import "sync"

// Event 是面板实时事件。InstanceID 为 0 表示与实例无关。
type Event struct {
	Type       string `json:"type"`
	InstanceID int64  `json:"instance_id,omitempty"`
	Payload    any    `json:"payload,omitempty"`
}

// Hub 是进程内事件广播器：多订阅者、慢消费者丢事件、不阻塞广播方。
type Hub struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

// NewHub 创建空的事件总线。
func NewHub() *Hub {
	return &Hub{subs: make(map[chan Event]struct{})}
}

// Subscribe 注册订阅者，返回只读通道与取消函数。
// cancel 仅把通道从订阅表中移除并交给 GC，不 close：Broadcast 可能正持读锁发送，close 会 panic。
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
	return ch, cancel
}

// Broadcast 向所有订阅者广播事件；慢消费者（缓冲满）直接丢弃，不阻塞。
func (h *Hub) Broadcast(e Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // 慢消费者丢事件，避免广播阻塞
		}
	}
}
