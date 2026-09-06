package utils

import (
	"sync"
)

type ipBroadcaster struct {
	mu      sync.Mutex
	clients map[chan string]bool // 存储双向 channel
}

var broadcaster = &ipBroadcaster{
	clients: make(map[chan string]bool),
}

// SubscribeIPChanges 订阅 IP 变化事件，返回一个 channel，每次 IP 变化时会收到 "ip:port" 字符串。调用者应只读取该 channel（但 channel 本身是双向的，以便内部发送）。
func SubscribeIPChanges() chan string {
	ch := make(chan string, 1)
	broadcaster.mu.Lock()
	broadcaster.clients[ch] = true
	// 发送当前值作为初始消息
	ip, port := GetCurrentLANInfo()
	if ip != "" {
		ch <- ip + ":" + port
	}
	broadcaster.mu.Unlock()
	return ch
}

// UnsubscribeIPChanges 取消订阅，关闭并移除 channel。
func UnsubscribeIPChanges(ch chan string) {
	broadcaster.mu.Lock()
	delete(broadcaster.clients, ch)
	broadcaster.mu.Unlock()
	close(ch) // 通知接收方不再有数据
}

// NotifyIPChanged 广播 IP 变化给所有订阅者。
func NotifyIPChanged(ip, port string) {
	msg := ip + ":" + port
	broadcaster.mu.Lock()
	clients := make([]chan string, 0, len(broadcaster.clients))
	for ch := range broadcaster.clients {
		clients = append(clients, ch)
	}
	broadcaster.mu.Unlock()

	for _, ch := range clients {
		select {
		case ch <- msg:
		default: // 非阻塞，防止慢消费者阻塞广播
		}
	}
}
