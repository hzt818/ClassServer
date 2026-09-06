package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"visualclassroom/classserver/config"
	"visualclassroom/classserver/database"

	"github.com/gofiber/fiber/v3"
)

//  语音转文字

const qwenASREndpoint = "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions"

var voiceTranscribeAPIKey string

func init() {
	cfg := config.LoadConfig()
	voiceTranscribeAPIKey = cfg.QwenAPIKey
}

type qwenASRInputAudio struct {
	Data string `json:"data"`
}

type qwenASRContentPart struct {
	Type       string             `json:"type"`
	InputAudio *qwenASRInputAudio `json:"input_audio,omitempty"`
}

type qwenASRMessage struct {
	Role    string               `json:"role"`
	Content []qwenASRContentPart `json:"content"`
}

type qwenASRRequest struct {
	Model    string           `json:"model"`
	Messages []qwenASRMessage `json:"messages"`
	Stream   bool             `json:"stream"`
}

type qwenASRResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// callQwenASR 把一段音频发给 Qwen-ASR，返回识别出的文字。
func callQwenASR(ctx context.Context, audioBase64, mimeType string) (string, error) {
	if voiceTranscribeAPIKey == "" {
		return "", fmt.Errorf("AI 服务未配置（config.properties 里的 qwen.api_key 为空）")
	}

	dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, audioBase64)
	reqBody := qwenASRRequest{
		Model: "qwen3-asr-flash",
		Messages: []qwenASRMessage{
			{
				Role: "user",
				Content: []qwenASRContentPart{
					{Type: "input_audio", InputAudio: &qwenASRInputAudio{Data: dataURI}},
				},
			},
		},
		Stream: false,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qwenASREndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+voiceTranscribeAPIKey)

	httpClient := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var parsed qwenASRResponse
	_ = json.Unmarshal(respBytes, &parsed) // 即使解析失败，用 HTTP 状态码报错

	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("语音识别接口返回错误: %s", parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("语音识别请求失败（HTTP %d）: %s", resp.StatusCode, string(respBytes))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("语音识别没有返回结果")
	}
	text := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if text == "" {
		return "", fmt.Errorf("未识别出文字内容（可能是静音，或所在地域/账号不支持这种音频格式）")
	}
	return text, nil
}

func mimeTypeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".webm":
		return "audio/webm"
	case ".ogg":
		return "audio/ogg"
	case ".mp4":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

// VoiceMessageTranscribe 发起一次语音转文字请求：立即创建一条"流式"占位消息并返回其 id，复用已有的 /api/messages/stream/:id 机制订阅结果。
func VoiceMessageTranscribe(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	msgID, err := strconv.Atoi(c.Params("messageId"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效消息ID"})
	}

	var content, msgType string
	var roomID int
	err = database.Pool.QueryRow(c.Context(),
		"SELECT content, msg_type, room_id FROM messages WHERE id=$1", msgID,
	).Scan(&content, &msgType, &roomID)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "消息不存在"})
	}
	if msgType != "voice_message" {
		return c.Status(400).JSON(fiber.Map{"error": "该消息不是语音消息"})
	}

	var payload VoiceMessagePayload
	if err := json.Unmarshal([]byte(content), &payload); err != nil || payload.Path == "" {
		return c.Status(500).JSON(fiber.Map{"error": "语音消息数据异常"})
	}

	const voiceFileURLPrefix = "/api/voice-message/file/"
	if !strings.HasPrefix(payload.Path, voiceFileURLPrefix) {
		return c.Status(500).JSON(fiber.Map{"error": "语音文件路径异常"})
	}
	filename := strings.TrimPrefix(payload.Path, voiceFileURLPrefix)
	localPath := filepath.Join(voiceMessageUploadDir, filename)
	audioBytes, err := os.ReadFile(localPath)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "语音文件不存在或已被清理"})
	}

	var transcriptID int
	err = database.Pool.QueryRow(c.Context(),
		"INSERT INTO messages (user_id, username, content, room_id, msg_type, is_streaming, reply_to_id) VALUES ($1, 'AI 助手', '', $2, 'voice_transcript', true, $3) RETURNING id",
		userID, roomID, msgID,
	).Scan(&transcriptID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	registerAIStream(transcriptID)

	mimeType := mimeTypeFromExt(filepath.Ext(filename))
	audioBase64 := base64.StdEncoding.EncodeToString(audioBytes)
	go streamVoiceTranscript(transcriptID, audioBase64, mimeType)

	return c.JSON(fiber.Map{"transcript_message_id": transcriptID, "source_message_id": msgID})
}

// streamVoiceTranscript 调用 Qwen-ASR，复用跟AI聊天回复一样的流式基础设施。
func streamVoiceTranscript(msgID int, audioBase64, mimeType string) {
	state := getAIStream(msgID)
	if state == nil {
		return
	}
	defer func() {
		state.finish()
		removeAIStream(msgID)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	text, err := callQwenASR(ctx, audioBase64, mimeType)
	if err != nil {
		errMsg := "语音识别失败：" + err.Error()
		state.append(errMsg)
		flushAIContent(msgID, errMsg, false)
		return
	}

	// Qwen-ASR 是一次性返回完整文本的，不是真流式接口，这里按小段陆续 append，配合前端已有的流式展示逻辑，观感和AI聊天回复一致。
	runes := []rune(text)
	const chunkSize = 3
	lastFlush := time.Now()
	const flushInterval = 120 * time.Millisecond
	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		state.append(string(runes[i:end]))
		if time.Since(lastFlush) >= flushInterval {
			flushAIContent(msgID, state.snapshot(), true)
			lastFlush = time.Now()
		}
		time.Sleep(35 * time.Millisecond)
	}
	flushAIContent(msgID, state.snapshot(), false)
}
