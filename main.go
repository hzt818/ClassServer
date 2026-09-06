package main

import (
	"visualclassroom/classserver/config"
	"visualclassroom/classserver/database"
	"visualclassroom/classserver/handlers"
	"visualclassroom/classserver/middleware"
	"visualclassroom/classserver/utils"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/static"
	"github.com/gofiber/template/html/v2"
)

const asciiArt = `
ooo        ooooo oooooo   oooo   .oooooo.   oooo
'88.       .888'  '888.   .8'   d8P'  'Y8b  '888
 888b     d'888    '888. .8'   888           888   .oooo.    .oooo.o  .oooo.o
 8 Y88. .P  888     '888.8'    888           888  'P  )88b  d88(  "8 d88(  "8
 8  '888'   888      '888'     888           888   .oP"888  '"Y88b.  '"Y88b.
 8    Y     888       888      '88b    ooo   888  d8(  888  o.  )88b o.  )88b
o8o        o888o     o888o      'Y8bood8P'  o888o 'Y888""8o 8""888P' 8""888P'
                                        MYClassroom · 我的课堂
--------------------------------------------------------------
`

// ---------- IP 检测 ----------

// getLocalIP 获取本机对外通信使用的局域网 IP。
// 短时间内探测 3 次，只采信出现次数最多的结果，过滤掉单次抖动
// （多网卡/VPN/虚拟网卡环境下路由表选择可能不稳定）。
func getLocalIP() string {
	const attempts = 3
	counts := make(map[string]int)
	for i := 0; i < attempts; i++ {
		if ip := probeOutboundIP(); ip != "" {
			counts[ip]++
		}
		if i < attempts-1 {
			time.Sleep(80 * time.Millisecond)
		}
	}
	best, bestCount := "", 0
	for ip, c := range counts {
		if c > bestCount {
			best, bestCount = ip, c
		}
	}
	if best == "" {
		log.Printf("🛑 无法获取对外通信IP")
	}
	return best
}

func probeOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// sanitizeWebURL 校验并重建一个只包含 https/http 协议、localhost 主机和数字端口的 URL。
// 自动打开浏览器属于便利功能，为杜绝把任何外部派生字符串带入进程执行接口，
// 这里只放行 "https://localhost:<0-65535>" 形式，其余一律返回空串。
func sanitizeWebURL(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return ""
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return ""
	}
	if u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		return ""
	}
	portNum, err := strconv.Atoi(u.Port())
	if err != nil || portNum < 1 || portNum > 65535 {
		return ""
	}
	return fmt.Sprintf("%s://localhost:%d/login", u.Scheme, portNum)
}

// openBrowser 跨平台打开系统默认浏览器到指定 URL。
// 只接受经过 sanitizeWebURL 白名单校验的 URL；进程调用一律使用参数数组、
// 不经过 shell（Windows 用 explorer 而不是 cmd /c start），不存在命令注入面。
// 失败只记录警告日志，不影响服务器主流程——自动打开浏览器是便利功能，不是关键路径。
func openBrowser(rawURL string) {
	url := sanitizeWebURL(rawURL)
	if url == "" {
		log.Printf("⚠️ URL 未通过白名单校验，跳过自动打开浏览器，请手动访问：%s", rawURL)
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", url)
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		log.Printf("⚠️ 未知操作系统 %s，无法自动打开浏览器，请手动访问：%s", runtime.GOOS, url)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("⚠️ 自动打开浏览器失败（不影响服务器运行）: %v，请手动访问：%s", err, url)
	}
}

// ---------- 清理任务 ----------
func cleanOldSubmissions() {
	ctx := context.Background()
	result, err := database.Pool.Exec(ctx,
		"DELETE FROM essay_submissions WHERE created_at < datetime('now', '-180 days')")
	if err != nil {
		log.Printf("🛑 清理旧作文记录失败: %v", err)
	} else {
		rows := result.RowsAffected()
		if rows > 0 {
			log.Printf("🧹 已清理 %d 条 180 天前的作文提交记录", rows)
		}
	}
}

func cleanOldFiles() {
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx,
		"SELECT id, filepath FROM chat_files WHERE is_deleted = true AND deleted_at < datetime('now', '-180 days')")
	if err != nil {
		log.Printf("🛑 查询待清理文件失败: %v", err)
		return
	}
	defer rows.Close()

	var ids []int
	var paths []string
	for rows.Next() {
		var id int
		var filepath string
		if err := rows.Scan(&id, &filepath); err == nil {
			ids = append(ids, id)
			paths = append(paths, filepath)
		}
	}
	if len(ids) == 0 {
		return
	}

	for _, p := range paths {
		localPath := "." + p
		if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
			log.Printf("⚠️ 删除物理文件 %s 失败: %v", localPath, err)
		}
	}

	tag, err := database.Pool.Exec(ctx,
		"DELETE FROM chat_files WHERE id = ANY($1)", ids)
	if err != nil {
		log.Printf("🛑 删除过期文件记录失败: %v", err)
		return
	}
	log.Printf("🧹 已清理 %d 条 180 天前删除的文件记录", tag.RowsAffected())
}

func cleanOldVoiceMessages() {
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx,
		"SELECT id, content FROM messages WHERE msg_type IN ('voice_message', 'voice_transcript') AND created_at < datetime('now', '-180 days')")
	if err != nil {
		log.Printf("🛑 查询待清理语音消息失败: %v", err)
		return
	}
	defer rows.Close()

	var ids []int
	var paths []string
	for rows.Next() {
		var id int
		var content string
		if err := rows.Scan(&id, &content); err != nil {
			continue
		}
		ids = append(ids, id)

		var payload struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(content), &payload); err == nil && payload.Path != "" {
			paths = append(paths, payload.Path)
		}
	}
	if len(ids) == 0 {
		return
	}
	const voiceFileURLPrefix = "/api/voice-message/file/"
	for _, p := range paths {
		if !strings.HasPrefix(p, voiceFileURLPrefix) {
			continue
		}
		filename := strings.TrimPrefix(p, voiceFileURLPrefix)
		localPath := "./static/uploads/voice/" + filename
		if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
			log.Printf("⚠️ 删除语音文件 %s 失败: %v", localPath, err)
		}
	}

	tag, err := database.Pool.Exec(ctx, "DELETE FROM messages WHERE id = ANY($1)", ids)
	if err != nil {
		log.Printf("🛑 删除过期语音消息记录失败: %v", err)
		return
	}
	log.Printf("🧹 已清理 %d 条 180 天前的语音消息记录", tag.RowsAffected())
}

// ---------- 证书管理 ----------
const certDir = "./certs"
const certFile = certDir + "/server.crt"
const keyFile = certDir + "/server.key"
const ipStoreFile = certDir + "/.last_ip"

var (
	certMu      sync.RWMutex
	currentCert tls.Certificate
	currentIP   string
)

func generateSelfSignedCert(ip string) (certPath, keyPath string, err error) {
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return "", "", err
	}

	lastIP := ""
	hasStoredIP := false
	if b, rerr := os.ReadFile(ipStoreFile); rerr == nil {
		lastIP = strings.TrimSpace(string(b))
		hasStoredIP = true
	}

	if lastIP == ip && ip != "" {
		if _, err := os.Stat(certFile); err == nil {
			if _, err := os.Stat(keyFile); err == nil {
				return certFile, keyFile, nil
			}
		}
	}

	switch {
	case ip == "":
		log.Println("⚠️ 无法获取 IP，证书将仅包含 localhost！")
	case !hasStoredIP:
		// 首次运行，本地从未记录过 IP，这不是"变化"，只是初始化，不发通知、不用变化措辞
		log.Printf("🔐 首次运行，正在为 IP %s 生成证书", ip)
	case lastIP != ip:
		log.Printf("🌐 检测到 IP 变化：%s → %s", lastIP, ip)
		log.Printf("⚠ 重新生成证书，请使用 %s 访问", ip)
		utils.SendSystemNotification("服务器 IP 已变化",
			fmt.Sprintf("%s → %s，证书已重新生成。请重启服务器后新 IP 才会生效。", lastIP, ip))
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"MYClassroom ClassServer"},
			CommonName:   "localhost",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	if ip != "" {
		if parsed := net.ParseIP(ip); parsed != nil {
			template.IPAddresses = append(template.IPAddresses, parsed)
		}
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return "", "", err
	}

	certOut, err := os.Create(certFile)
	if err != nil {
		return "", "", err
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certBytes}); err != nil {
		return "", "", err
	}

	keyOut, err := os.Create(keyFile)
	if err != nil {
		return "", "", err
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}); err != nil {
		return "", "", err
	}

	if err := os.WriteFile(ipStoreFile, []byte(ip), 0644); err != nil {
		log.Printf("⚠️ 保存IP记录失败: %v", err)
	}

	log.Printf("🔐 已成功生成自签名证书（有效期一年），包含IP：%s", ip)
	return certFile, keyFile, nil
}

func loadCertificate(certPath, keyPath string) error {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return err
	}
	certMu.Lock()
	currentCert = cert
	certMu.Unlock()
	return nil
}

func getTLSConfig() *tls.Config {
	certMu.RLock()
	defer certMu.RUnlock()
	return &tls.Config{
		Certificates: []tls.Certificate{currentCert},
		MinVersion:   tls.VersionTLS12,
	}
}

// startIPMonitor 定期检查 IP，若变化则重新生成证书。
// 检测到候选变化后立即做一次几秒内的快速二次探测来确认，
// 而不是等待下一个完整的轮询周期，兼顾了"过滤单次抖动"和"尽快发现真实变化"。
func startIPMonitor(app *fiber.App, port string) {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		lastIP := getLocalIP()
		for range ticker.C {
			newIP := getLocalIP()
			if newIP == "" || newIP == lastIP {
				continue
			}

			// 候选变化：短暂等待后立刻再探测一次做二次确认，而不是等下一个 tick
			time.Sleep(3 * time.Second)
			confirmIP := getLocalIP()
			if confirmIP != newIP {
				continue
			}

			log.Printf("🌐 运行时检测到 IP 变化：%s → %s，正在重新生成证书...", lastIP, newIP)
			certPath, keyPath, err := generateSelfSignedCert(newIP)
			if err != nil {
				log.Printf("🛑 重新生成证书失败: %v", err)
				continue
			}
			if err := loadCertificate(certPath, keyPath); err != nil {
				log.Printf("🛑 加载新证书失败: %v", err)
				continue
			}
			log.Printf("✅ 证书已更新，但需要重启服务器才能使新 IP 生效。建议手动重启程序。")
			utils.SendSystemNotification("服务器 IP 已变化",
				fmt.Sprintf("%s → %s，证书已更新，需要重启服务器后新 IP 才会生效。", lastIP, newIP))
			utils.SetCurrentLANInfo(newIP, port)
			lastIP = newIP
		}
	}()
}

// ---------- main ----------
func main() {
	cfg := config.LoadConfig()
	config.EnsureJWTSecret(cfg)
	utils.SetJWTSecret(cfg.JWTSecret)

	if cfg.Timezone != "" {
		if loc, err := time.LoadLocation(cfg.Timezone); err == nil {
			time.Local = loc
		} else {
			log.Printf("🛑 时区 %q 无效，继续使用系统默认时区: %v", cfg.Timezone, err)
		}
	}

	if err := database.InitDB(cfg); err != nil {
		log.Fatalf("🛑 数据库初始化失败: %v", err)
	}
	defer database.CloseDB()

	if err := database.SeedUsers(cfg.SeedDemoAccounts); err != nil {
		log.Fatalf("🛑 初始化数据失败: %v", err)
	}

	go func() {
		cleanOldSubmissions()
		cleanOldFiles()
		cleanOldVoiceMessages()
		handlers.CleanOrphanEssayImages()
		ticker := time.NewTicker(180 * 24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			cleanOldSubmissions()
			cleanOldFiles()
			cleanOldVoiceMessages()
			handlers.CleanOrphanEssayImages()
		}
	}()

	// 孤儿原图清理频率更高一些（草稿被取消/覆盖后留下的文件，不必等 180 天）
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			handlers.CleanOrphanEssayImages()
		}
	}()

	// 语音通话心跳超时清理（覆盖"直接关标签页/断网，从未调用 /leave"的场景）
	handlers.StartVoiceTimeoutSweeper()

	templatesSub, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		log.Fatalf("🛑 加载内嵌模板失败: %v", err)
	}
	engine := html.NewFileSystem(http.FS(templatesSub), ".html")

	app := fiber.New(fiber.Config{Views: engine})

	app.Use(logger.New(logger.Config{Stream: devConsoleOutput}))
	app.Use(recover.New())
	app.Use(devConsoleHitMiddleware)

	// HTML 页面禁用浏览器缓存：模板改版后必须立即对用户可见，
	// 否则浏览器启发式缓存会继续提供旧页面（静态资源仍走版本号缓存策略）
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		if err == nil && strings.HasPrefix(string(c.Response().Header.ContentType()), "text/html") {
			c.Response().Header.Set("Cache-Control", "no-cache, must-revalidate")
			c.Response().Header.Set("Pragma", "no-cache")
		}
		return err
	})

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("🛑 加载内嵌静态文件失败: %v", err)
	}
	app.Use("/static", static.New("", static.Config{FS: staticSub}))

	// ---- 路由 ----
	app.Get("/", func(c fiber.Ctx) error { return c.Redirect().To("/login") })
	app.Get("/favicon.ico", func(c fiber.Ctx) error {
		data, err := staticFS.ReadFile("static/images/favicon.ico")
		if err != nil {
			return c.Status(fiber.StatusNotFound).SendString("favicon not found")
		}
		c.Set("Content-Type", "image/x-icon")
		c.Set("Cache-Control", "public, max-age=86400")
		return c.Send(data)
	})
	app.Get("/login", handlers.ShowLogin)
	app.Post("/login", handlers.HandleLogin)
	app.Get("/logout", handlers.Logout)

	// 自助注册 + 教师支付宝实名认证（未登录可访问；身份证号仅内存态，不落库）
	app.Get("/register", handlers.ShowRegister)
	app.Post("/api/register", handlers.HandleRegister)
	app.Get("/api/certify/start", handlers.StartCertify)
	app.Get("/api/certify/query", handlers.QueryCertify)

	// AI 教学工具（登录即可用；评语/小结仅教师）
	app.Post("/api/ai/comment", middleware.AuthRequired, handlers.AiStudentComment)
	app.Post("/api/ai/error-analysis", middleware.AuthRequired, handlers.AiErrorAnalysis)
	app.Post("/api/ai/lesson-summary", middleware.AuthRequired, handlers.AiLessonSummary)

	app.Get("/api/lan-info", handlers.GetLANInfo)

	// SSE 实时推送
	app.Get("/api/lan-info/stream", handlers.IPChangeStream)

	app.Get("/dashboard", middleware.AuthRequired, func(c fiber.Ctx) error {
		role := c.Locals("role").(string)
		if role == "teacher" {
			return c.Redirect().To("/teacher/dashboard")
		}
		return c.Redirect().To("/student/dashboard")
	})
	app.Get("/chat", middleware.AuthRequired, handlers.ShowChat)
	app.Get("/essay", middleware.AuthRequired, handlers.ShowEssay)
	app.Get("/pipeline", middleware.AuthRequired, middleware.RoleRequired("teacher"), handlers.ShowPipeline)
	app.Get("/showcase", middleware.AuthRequired, handlers.ShowShowcase)
	app.Get("/cast", middleware.AuthRequired, handlers.ShowCast)
	app.Get("/about", middleware.AuthRequired, handlers.About)
	app.Get("/admin/users", middleware.AuthRequired, middleware.RoleRequired("teacher"), handlers.ShowAdminUsers)

	api := app.Group("/api", middleware.AuthRequired)
	{
		api.Get("/me", handlers.GetCurrentUser)

		api.Get("/rooms", handlers.GetRooms)
		api.Post("/rooms", handlers.CreateRoom)
		api.Put("/rooms/:id", handlers.RenameRoom)
		api.Delete("/rooms/:id", handlers.DeleteRoom)
		api.Post("/rooms/:id/restore", handlers.RestoreRoom)

		api.Get("/rooms/:roomId/messages", handlers.GetMessages)
		api.Post("/messages", handlers.SendMessage)
		api.Post("/messages/recall", handlers.RecallMessage)
		api.Post("/messages/teacher_delete", handlers.TeacherDeleteMessage)
		api.Get("/messages/stream/:id", handlers.StreamAIMessage)
		api.Post("/messages/:id/reaction", handlers.ToggleReaction)

		api.Get("/mentions/pending", handlers.GetPendingMentions)
		api.Post("/mentions/seen", handlers.MarkMentionsSeen)

		api.Post("/essay/ocr", handlers.HandleOCR)
		api.Post("/essay/submit", handlers.CreateEssayJob)
		api.Get("/essay/stream/:id", handlers.StreamEssayJob)
		api.Get("/essay/jobs", handlers.GetEssayJobs)
		api.Post("/essay/jobs/:id/pause", handlers.PauseEssayJob)
		api.Get("/essay/jobs/:id/image", handlers.GetEssayJobImage)
		api.Get("/essay/jobs/:id/annotated", handlers.GetEssayJobAnnotatedImage)
		api.Get("/essay/jobs/:id/annotations", handlers.GetEssayAnnotations)
		api.Post("/essay/jobs/:id/excellent", middleware.RoleRequired("teacher"), handlers.ToggleEssayExcellent)
		api.Put("/essay/jobs/:id/short-comment", middleware.RoleRequired("teacher"), handlers.SetEssayShortComment)
		api.Post("/essay/jobs/:id/regrade", middleware.RoleRequired("teacher"), handlers.RegradeEssayJob)
		api.Delete("/essay/jobs", handlers.DeleteEssayJobs)
		api.Put("/essay/jobs/:id/score", handlers.UpdateEssayJobScore)
		api.Get("/essay/drafts", handlers.GetEssayDrafts)
		api.Delete("/essay/drafts", handlers.DeleteEssayDrafts)
		api.Get("/essay/submissions", handlers.GetSubmissions)
		api.Post("/essay/word-export", handlers.ExportEssayReports) // docx（默认）/ pdf
		api.Post("/essay/export", handlers.ExportEssayReports)
		api.Post("/essay/model-essay", handlers.GenerateModelEssay)
		api.Get("/essay/excellent", handlers.ListExcellentEssays)
		api.Get("/essay/pipeline", middleware.RoleRequired("teacher"), handlers.GetPipelineJobs)
		api.Get("/essay/students", middleware.RoleRequired("teacher"), handlers.GetStudentStats)

		// 课堂传屏：教师推流 / 学生拉流 / 录屏存档
		api.Post("/cast/frame", middleware.RoleRequired("teacher"), handlers.CastFrame)
		api.Post("/cast/stop", middleware.RoleRequired("teacher"), handlers.CastStop)
		api.Get("/cast/live", handlers.CastLive)
		api.Get("/cast/status", handlers.CastStatus)
		api.Post("/cast/record/start", middleware.RoleRequired("teacher"), handlers.CastRecordStart)
		api.Post("/cast/record/chunk", middleware.RoleRequired("teacher"), handlers.CastRecordChunk)
		api.Post("/cast/record/finish", middleware.RoleRequired("teacher"), handlers.CastRecordFinish)
		api.Get("/cast/recordings", middleware.RoleRequired("teacher"), handlers.CastRecordings)
		api.Get("/cast/recordings/:id/download", middleware.RoleRequired("teacher"), handlers.CastRecordingDownload)
		api.Delete("/cast/recordings/:id", middleware.RoleRequired("teacher"), handlers.CastRecordingDelete)

		api.Get("/admin/users", middleware.RoleRequired("teacher"), handlers.GetUsers)
		api.Post("/admin/users", middleware.RoleRequired("teacher"), handlers.CreateUsers)
		api.Put("/admin/users/:id", middleware.RoleRequired("teacher"), handlers.UpdateUser)
		api.Delete("/admin/users", middleware.RoleRequired("teacher"), handlers.DeleteUsers)
		api.Delete("/admin/users/:id", middleware.RoleRequired("teacher"), handlers.DeleteUser)
		api.Post("/admin/users/rename", middleware.RoleRequired("teacher"), handlers.RenameUsers)
		api.Post("/admin/users/:id/avatar", middleware.RoleRequired("teacher"), handlers.UploadAvatar)
		api.Get("/admin/export/records", middleware.RoleRequired("teacher"), handlers.ExportRecords)

		api.Get("/users", handlers.GetChatUsers)

		api.Get("/chat_time_rules", handlers.GetChatTimeRulesPublic)
		api.Get("/admin/chat_time_rules", middleware.RoleRequired("teacher"), handlers.GetChatTimeRules)
		api.Post("/admin/chat_time_rules", middleware.RoleRequired("teacher"), handlers.SetChatTimeRules)

		api.Get("/files/:roomId", handlers.GetFiles)
		api.Post("/files/upload", handlers.UploadFile)
		api.Get("/files/download/:fileId", handlers.DownloadFile)
		api.Put("/files/:fileId/access", handlers.UpdateFileAccess)
		api.Delete("/files/:fileId", handlers.DeleteFile)
		api.Post("/files/:fileId/restore", handlers.RestoreFile)

		api.Post("/voice/:roomId/invite", handlers.VoiceInvite)
		api.Get("/voice/:roomId/stream", handlers.VoiceStream)
		api.Post("/voice/:roomId/join", handlers.VoiceJoin)
		api.Post("/voice/:roomId/leave", handlers.VoiceLeave)
		api.Post("/voice/:roomId/signal", handlers.VoiceSignal)
		api.Post("/voice/:roomId/state", handlers.VoiceState)
		api.Post("/voice/:roomId/heartbeat", handlers.VoiceHeartbeat)

		api.Post("/voice-message/upload", handlers.VoiceMessageUpload)
		api.Get("/voice-message/file/:filename", handlers.VoiceMessageFile)
		api.Post("/voice-message/:messageId/transcribe", handlers.VoiceMessageTranscribe)
	}

	teacherGroup := app.Group("/teacher", middleware.AuthRequired, middleware.RoleRequired("teacher"))
	teacherGroup.Get("/dashboard", handlers.TeacherDashboard)

	studentGroup := app.Group("/student", middleware.AuthRequired, middleware.RoleRequired("student"))
	studentGroup.Get("/dashboard", handlers.StudentDashboard)

	// ---- 证书初始化 ----
	ip := getLocalIP()
	if ip == "" {
		log.Println("⚠️ 无法获取本机 IP，证书将仅包含 localhost 和 127.0.0.1")
	}
	certPath, keyPath, err := generateSelfSignedCert(ip)
	if err != nil {
		log.Fatalf("🛑 生成自签名证书失败: %v", err)
	}
	if err := loadCertificate(certPath, keyPath); err != nil {
		log.Fatalf("🛑 加载证书失败: %v", err)
	}

	port := cfg.Port
	if port == "" {
		port = "8900"
	}
	utils.SetCurrentLANInfo(ip, port)

	// 创建 TLS 监听器
	listener, err := tls.Listen("tcp", ":"+port, getTLSConfig())
	if err != nil {
		msg := fmt.Sprintf("MYClassroom 教室服务站启动失败：\n\n端口 %s 无法监听（可能已被占用）。\n\n请检查：\n1. 是否已有一个教室服务站实例在运行（任务管理器结束 ClassServer.exe）；\n2. 其它程序是否占用了 %s 端口；\n3. 如需换端口，修改 config.properties 中的 app.port 后重启。\n\n详细错误：%v", port, port, err)
		utils.ShowErrorDialog("MYClassroom 启动失败", msg)
		log.Fatalf("🛑 创建 TLS 监听器失败: %v", err)
	}
	defer listener.Close()

	// 启动 IP 变化监控
	startIPMonitor(app, port)

	// 打印启动信息
	log.Printf("🚀 服务器正在监听 HTTPS 端口 %s", port)
	if ip != "" {
		log.Printf("🌐 局域网访问：https://%s:%s（请确保防火墙允许此端口：%s并且在输入时一定要有'https://'！！！）", ip, port, port)
	}
	log.Printf("🌐 本地访问：https://localhost:%s（在输入时一定要有'https://'！！！）", port)
	log.Print(asciiArt)

	startDevConsole(cfg)

	// 启动服务器
	go func() {
		if err := app.Listener(listener); err != nil {
			log.Fatalf("🛑 启动 HTTPS 服务器失败: %v", err)
		}
	}()

	// 给服务器一点时间完成启动，然后自动打开默认浏览器直接跳转到登录页。
	// 双击运行后不需要再手动打开浏览器、输入网址——教师端全程不用关心网址是什么。
	go func() {
		time.Sleep(500 * time.Millisecond)
		openBrowser(fmt.Sprintf("https://localhost:%s/login", port))
	}()

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("⏳ 正在关闭服务器，请稍等...")
	if err := app.Shutdown(); err != nil {
		log.Printf("🛑 服务器关闭错误: %v", err)
	}
	log.Println("✅ 服务器已安全关闭")
}
