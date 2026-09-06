// Copyright (c) VisualClassroom. All rights reserved.

// 千问（DashScope）客户端：OpenAI 兼容模式。
//
// 模型分级（config 可覆盖，见 config.properties 的 qwen.model）：
//   - 文本主力   qwen-max            旗舰，批改/点评质量优先
//   - 视觉主力   qwen-vl-max-latest  OCR / 卷面点评
//   - 兜底       qwen-plus           主力失败时的降级模型
//
// 客户端带超时；CreateChatCompletion 带一次自动重试（网络抖动 / 限流 429）。
package handlers

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"visualclassroom/classserver/config"

	"github.com/sashabaranov/go-openai"
)

var (
	qwenClient *openai.Client

	qwenTextModel     string // 文本主力模型
	qwenVisionModel   string // 视觉主力模型
	qwenFallbackModel string // 文本降级模型
	qwenAPIKeyed      bool   // 是否已配置 Key
)

func init() {
	cfg := config.LoadConfig()
	if cfg.QwenAPIKey == "" {
		log.Println("⚠️  未配置 qwen.api_key：AI 批改 / 聊天助手 / OCR 均不可用（config.properties）")
		return
	}
	openaiConfig := openai.DefaultConfig(cfg.QwenAPIKey)
	openaiConfig.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	// 客户端级超时：批改/OCR 属长请求，给足余量
	openaiConfig.HTTPClient = &http.Client{Timeout: 180 * time.Second}
	qwenClient = openai.NewClientWithConfig(openaiConfig)

	qwenTextModel = pickModel(cfg.QwenTextModel, "qwen3.8-flash")
	qwenVisionModel = pickModel(cfg.QwenVisionModel, "qwen3-vl-plus")
	qwenFallbackModel = pickModel(cfg.QwenFallbackModel, "qwen3.8-flash")
	qwenAPIKeyed = true
	log.Printf("✅ 千问 AI 就绪（文本 %s / 视觉 %s / 兜底 %s）", qwenTextModel, qwenVisionModel, qwenFallbackModel)
}

// pickModel 取配置值，空则用默认；空白安全。
func pickModel(val, def string) string {
	if v := strings.TrimSpace(val); v != "" {
		return v
	}
	return def
}

// qwenErrUnavailable 未配置 Key 时的统一错误文案。
func qwenErrUnavailable() string {
	return "AI 未配置：请在 config.properties 填写 qwen.api_key 后重启服务"
}

// qwenCreateChat 带一次重试的非流式调用；4xx 业务错误不重试。
func qwenCreateChat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	if req.Model == "" {
		req.Model = qwenTextModel
	}
	resp, err := qwenClient.CreateChatCompletion(ctx, req)
	if err == nil {
		return resp, nil
	}
	if isRetryableQwenErr(err) {
		log.Printf("千问调用失败将重试一次 (%s): %v", req.Model, err)
		time.Sleep(800 * time.Millisecond)
		return qwenClient.CreateChatCompletion(ctx, req)
	}
	return resp, err
}

// chatRequest 组装一条"系统提示词 + 用户输入"的标准文本请求。
func chatRequest(systemPrompt, userInput string) openai.ChatCompletionRequest {
	return openai.ChatCompletionRequest{
		Model: qwenTextModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userInput},
		},
	}
}

// isRetryableQwenErr 仅对网络错误 / 限流 / 5xx 重试。
func isRetryableQwenErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"timeout", "connection", "eof", "429", "500", "502", "503", "504", "rate limit"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
