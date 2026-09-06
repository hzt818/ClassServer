package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"visualclassroom/classserver/config"
	"visualclassroom/classserver/database"

	"github.com/gofiber/fiber/v3"
)

var (
	devConsoleHits      int64
	devConsoleBytesIn   int64
	devConsoleBytesOut  int64
	devConsoleStartTime = time.Now()
)

type devConsoleWriter struct {
	mu           sync.Mutex
	promptActive int32
}

func (w *devConsoleWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	active := atomic.LoadInt32(&w.promptActive) == 1
	if active {
		fmt.Fprint(os.Stdout, "\r\033[2K")
	}
	n, err := os.Stdout.Write(p)
	if active {
		fmt.Fprint(os.Stdout, "> ")
	}
	return n, err
}

func (w *devConsoleWriter) setPromptActive(active bool) {
	if active {
		atomic.StoreInt32(&w.promptActive, 1)
	} else {
		atomic.StoreInt32(&w.promptActive, 0)
	}
}

var devConsoleOutput = &devConsoleWriter{}

func init() {
	log.SetOutput(devConsoleOutput)
}

func devConsoleHitMiddleware(c fiber.Ctx) error {
	atomic.AddInt64(&devConsoleHits, 1)
	atomic.AddInt64(&devConsoleBytesIn, int64(len(c.Body())))
	err := c.Next()
	atomic.AddInt64(&devConsoleBytesOut, int64(len(c.Response().Body())))
	return err
}

func printDevConsolePrompt() {
	fmt.Fprint(devConsoleOutput, "> ")
	devConsoleOutput.setPromptActive(true)
}

func startDevConsole(cfg *config.Config) {
	if !cfg.ConsoleDev {
		return
	}
	go func() {
		reader := bufio.NewReader(os.Stdin)
		fmt.Fprintln(devConsoleOutput, "🛠️  开发者调试控制台已开启（console.dev=true）。输入 help 查看可用命令。")
		printDevConsolePrompt()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				// 不影响服务器主流程。
				return
			}
			devConsoleOutput.setPromptActive(false)
			cmd := strings.TrimSpace(line)
			if cmd != "" {
				handleDevConsoleCommand(cmd)
			}
			printDevConsolePrompt()
		}
	}()
}

func handleDevConsoleCommand(cmd string) {
	switch strings.ToLower(cmd) {
	case "help", "?":
		fmt.Fprintln(devConsoleOutput, "可用命令：")
		fmt.Fprintln(devConsoleOutput, "  occupy        查看数据库连接、upload 文件夹大小、CPU/内存、网络使用情况等运行状态快照")
		fmt.Fprintln(devConsoleOutput, "  help / ?      显示本帮助")
	case "occupy":
		printOccupyReport()
	default:
		fmt.Fprintf(devConsoleOutput, "未知命令：%q，输入 help 查看可用命令\n", cmd)
	}
}

func printOccupyReport() {
	fmt.Fprintln(devConsoleOutput, "──────────── occupy · 运行状态快照 ────────────")

	// SQLite（database/sql）连接池统计：InUse≈使用中、Idle≈空闲、WaitCount≈累计等待获取次数
	if database.Pool != nil {
		stat := database.Pool.Stats()
		fmt.Fprintf(devConsoleOutput, "[数据库]  累计等待获取次数：%d  |  最大打开连接上限：%d（空闲 %d / 使用中 %d）\n",
			stat.WaitCount, stat.MaxOpenConnections, stat.Idle, stat.InUse)
	} else {
		fmt.Fprintln(devConsoleOutput, "[数据库]  连接池尚未初始化")
	}

	uploadSize, fileCount, err := dirSize("./static/uploads")
	if err != nil {
		fmt.Fprintf(devConsoleOutput, "[存储]    读取 upload 文件夹失败：%v\n", err)
	} else {
		fmt.Fprintf(devConsoleOutput, "[存储]    static/uploads 总大小：%s（%d 个文件）\n", humanBytes(uploadSize), fileCount)
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	fmt.Fprintf(devConsoleOutput, "[CPU]     CPU 核心数：%d  |  GOMAXPROCS：%d  |  当前 Goroutine 数：%d\n",
		runtime.NumCPU(), runtime.GOMAXPROCS(0), runtime.NumGoroutine())
	fmt.Fprintf(devConsoleOutput, "[内存]    进程已分配：%s  |  已从系统申请：%s  |  累计 GC 次数：%d\n",
		humanBytes(int64(mem.Alloc)), humanBytes(int64(mem.Sys)), mem.NumGC)

	hits := atomic.LoadInt64(&devConsoleHits)
	bytesIn := atomic.LoadInt64(&devConsoleBytesIn)
	bytesOut := atomic.LoadInt64(&devConsoleBytesOut)
	fmt.Fprintf(devConsoleOutput, "[网络]    HTTP 请求总数：%d  |  入站约：%s  |  出站约：%s（HTTP 层估算，非网卡统计）\n",
		hits, humanBytes(bytesIn), humanBytes(bytesOut))

	fmt.Fprintf(devConsoleOutput, "[进程]    已运行：%s\n", time.Since(devConsoleStartTime).Round(time.Second))
	fmt.Fprintln(devConsoleOutput, "────────────────────────────────────────────")
}

func dirSize(root string) (int64, int, error) {
	var total int64
	var count int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			total += info.Size()
			count++
		}
		return nil
	})
	if err != nil && os.IsNotExist(err) {
		return 0, 0, nil
	}
	return total, count, err
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
