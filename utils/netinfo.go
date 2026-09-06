package utils

import "sync"

var (
	lanInfoMu sync.RWMutex
	lanIP     string
	lanPort   string
)

// SetCurrentLANInfo 由 main 包在服务启动、以及 IP 发生变化并重新生成证书之后调用，更新当前对外可访问的局域网 IP 和端口。
func SetCurrentLANInfo(ip, port string) {
	lanInfoMu.Lock()
	lanIP = ip
	lanPort = port
	lanInfoMu.Unlock()
	NotifyIPChanged(ip, port)
}

// GetCurrentLANInfo 供 handlers 包（登录页等）读取当前局域网访问地址，用于在页面上直接展示给用户，不需要用户自己去看控制台日志。
func GetCurrentLANInfo() (ip, port string) {
	lanInfoMu.RLock()
	defer lanInfoMu.RUnlock()
	return lanIP, lanPort
}
