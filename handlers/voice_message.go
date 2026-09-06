package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"visualclassroom/classserver/database"

	"github.com/gofiber/fiber/v3"
)

//  语音输入消息

const voiceMessageUploadDir = "./static/uploads/voice"

// VoiceMessagePayload 存放在 messages.content 里的 JSON 结构
type VoiceMessagePayload struct {
	Path     string `json:"path"`
	Duration int    `json:"duration"` // 秒，向上取整
}

func ensureVoiceMessageDir() error {
	return os.MkdirAll(voiceMessageUploadDir, 0755)
}

// extFromContentType 根据浏览器 MediaRecorder 常见的 mimeType 推断文件后缀
func extFromContentType(ct string) string {
	ct = strings.ToLower(ct)
	switch {
	case strings.Contains(ct, "webm"):
		return ".webm"
	case strings.Contains(ct, "ogg"):
		return ".ogg"
	case strings.Contains(ct, "mp4"):
		return ".mp4"
	case strings.Contains(ct, "mpeg") || strings.Contains(ct, "mp3"):
		return ".mp3"
	case strings.Contains(ct, "wav"):
		return ".wav"
	default:
		return ".webm"
	}
}

// VoiceMessageUpload 接收录音文件，落盘并插入一条语音消息
func VoiceMessageUpload(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	username := c.Locals("username").(string)

	roomIDStr := c.FormValue("room_id")
	roomID, err := strconv.Atoi(roomIDStr)
	if err != nil || roomID <= 0 {
		return c.Status(400).JSON(fiber.Map{"error": "无效房间ID"})
	}

	durationF, err := strconv.ParseFloat(c.FormValue("duration"), 64)
	if err != nil || durationF <= 0 {
		return c.Status(400).JSON(fiber.Map{"error": "无效的录音时长"})
	}
	durationSec := int(durationF + 0.5) // 四舍五入
	if durationSec < 1 {
		durationSec = 1
	}
	if durationSec > 65 { // 前端限制 60s，留 5s 容错，超出视为异常
		return c.Status(400).JSON(fiber.Map{"error": "语音消息最长 60 秒"})
	}

	var exists bool
	_ = database.Pool.QueryRow(c.Context(), "SELECT EXISTS(SELECT 1 FROM chat_rooms WHERE id=$1 AND is_deleted=false)", roomID).Scan(&exists)
	if !exists {
		return c.Status(404).JSON(fiber.Map{"error": "房间不存在"})
	}

	fileHeader, err := c.FormFile("audio")
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "未收到录音文件"})
	}
	if fileHeader.Size <= 0 || fileHeader.Size > 20*1024*1024 { // 20MB 上限，防止异常大文件
		return c.Status(400).JSON(fiber.Map{"error": "录音文件大小异常"})
	}

	if err := ensureVoiceMessageDir(); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "服务器存储初始化失败: " + err.Error()})
	}

	ext := extFromContentType(fileHeader.Header.Get("Content-Type"))
	filename := fmt.Sprintf("%d_%d%s", userID, time.Now().UnixNano(), ext)
	dstPath := filepath.Join(voiceMessageUploadDir, filename)

	if err := c.SaveFile(fileHeader, dstPath); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "保存录音失败: " + err.Error()})
	}

	payload := VoiceMessagePayload{
		Path:     "/api/voice-message/file/" + filename,
		Duration: durationSec,
	}
	contentBytes, _ := json.Marshal(payload)

	var msgID int
	err = database.Pool.QueryRow(c.Context(),
		`INSERT INTO messages (user_id, username, content, room_id, msg_type)
		 VALUES ($1, $2, $3, $4, 'voice_message') RETURNING id`,
		userID, username, string(contentBytes), roomID,
	).Scan(&msgID)
	if err != nil {
		_ = os.Remove(dstPath)
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}

	return c.JSON(fiber.Map{"id": msgID, "room_id": roomID, "path": payload.Path, "duration": payload.Duration})
}

// VoiceMessageFile 提供语音消息音频文件的下载/播放
func VoiceMessageFile(c fiber.Ctx) error {
	// 需要登录态即可播放（与聊天室其它资源权限一致），不做房间级校验，因为文件名本身包含发送者 ID + 时间戳。
	filename := filepath.Base(c.Params("filename")) // 防路径穿越
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		return c.Status(400).JSON(fiber.Map{"error": "无效文件名"})
	}
	fullPath := filepath.Join(voiceMessageUploadDir, filename)
	if _, err := os.Stat(fullPath); err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "语音文件不存在"})
	}

	ext := strings.ToLower(filepath.Ext(filename))
	contentType := "application/octet-stream"
	switch ext {
	case ".webm":
		contentType = "audio/webm"
	case ".ogg":
		contentType = "audio/ogg"
	case ".mp4":
		contentType = "audio/mp4"
	case ".mp3":
		contentType = "audio/mpeg"
	case ".wav":
		contentType = "audio/wav"
	}
	c.Set("Content-Type", contentType)
	c.Set("Cache-Control", "private, max-age=86400")
	return c.SendFile(fullPath)
}
