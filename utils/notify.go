package utils

import (
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"
)

// SendSystemNotification 发出一条系统级通知。
//
// 安全与依赖取舍：本服务运行在教师机上，通知只承载「IP 变化」等运维事件。
// 为避免任何进程派生/脚本注入面（并省去内嵌第三方通知器），这里采用
// 控制台醒目横幅输出；关键事件同时会由登录页的局域网地址展示（SSE 实时推送）
// 与 VisualClassroom 主程序的服务状态面板感知，不依赖系统 Toast。
func SendSystemNotification(title, message string) {
	line := "─" + strings.Repeat("─", 60)
	log.Printf("\n%s\n📢 %s\n   %s\n%s", line, title, message, line)

	// 同步给订阅方（未来可挂接 Web 控制台事件流）
	notifySubscribersMu.Lock()
	for _, ch := range notifySubscribers {
		select {
		case ch <- title + ": " + message:
		default:
		}
	}
	notifySubscribersMu.Unlock()
}

var (
	notifySubscribersMu sync.Mutex
	notifySubscribers   []chan string
)

// SubscribeNotifications 订阅系统通知事件（非阻塞广播）。
func SubscribeNotifications() chan string {
	ch := make(chan string, 8)
	notifySubscribersMu.Lock()
	notifySubscribers = append(notifySubscribers, ch)
	notifySubscribersMu.Unlock()
	return ch
}

// UnsubscribeNotifications 取消订阅。
func UnsubscribeNotifications(ch chan string) {
	notifySubscribersMu.Lock()
	for i, c := range notifySubscribers {
		if c == ch {
			notifySubscribers = append(notifySubscribers[:i], notifySubscribers[i+1:]...)
			break
		}
	}
	notifySubscribersMu.Unlock()
	close(ch)
}

// NotifyNow 供外部（如 dev 控制台）直接发起一条通知。
func NotifyNow(title, message string) {
	_ = runtime.GOOS // 平台差异在纯日志实现下不再需要分支，保留签名稳定
	go SendSystemNotification(title, fmt.Sprintf("%s（%s）", message, time.Now().Format("15:04:05")))
}
