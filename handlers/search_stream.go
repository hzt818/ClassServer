// Copyright (c) 黄智韬. All rights reserved.

// 带联网搜索（enable_search）的流式对话：直接调用 DashScope OpenAI 兼容端点，
// 自解析 SSE。用于作文批改——模型对不确定的知识点（语法规则、典故出处、
// 表达地道性等）可联网核实后再下评语。go-openai 库不透传 enable_search，
// 故批改流式走这里；其它 AI 调用仍走 go-openai。
package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"visualclassroom/classserver/config"
)

// qwenMaxOutputTokens 显式抬高单次输出上限：兼容模式默认上限较小，
// 长批改（意见 + 修改版 + 升格指导 + 升格范文 + 总分）会被中途截断，
// 造成"有的报告有升格范文、有的没有"且总分行丢失。qwen 文本模型
// 输出上限普遍为 8192，这里显式请求满额。
const qwenMaxOutputTokens = 8192

// qwenSearchEnabled 是否开启联网搜索（config: qwen.search，默认 true）。
var qwenSearchEnabled = true

func init() {
	qwenSearchEnabled = config.LoadConfig().QwenSearch
}

// 流式批改可能持续很久（读后续写 + 联网搜索 + 两个修改版），
// Timeout 覆盖整个响应体的读取：给足 15 分钟，避免长批改被掐断后
// 进入"重试→从头再生成"的假死循环；提前取消仍由 ctx 控制。
var searchHTTPClient = &http.Client{Timeout: 900 * time.Second}

// streamChatWithSearch 发起带 enable_search 的流式对话（单轮）。
// 返回流的结束原因：正常结束返回 ""；被 max_tokens 截断返回 "length"，
// 调用方可据此发起续写。
func streamChatWithSearch(ctx context.Context, systemPrompt, userContent string, search bool, temperature float64, onDelta func(string)) (string, error) {
	messages := []map[string]string{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": userContent},
	}
	return streamChatMessages(ctx, messages, search, temperature, onDelta)
}

// streamChatMessages 带完整消息数组的流式对话（供截断续写复用）。
func streamChatMessages(ctx context.Context, messages []map[string]string, search bool, temperature float64, onDelta func(string)) (string, error) {
	cfg := config.LoadConfig()
	body := map[string]any{
		"model":       qwenTextModel,
		"messages":    messages,
		"stream":      true,
		"temperature": clampTemperature(temperature),
		"max_tokens":  qwenMaxOutputTokens,
	}
	if search {
		body["enable_search"] = true
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions",
		bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.QwenAPIKey)

	resp, err := searchHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 读取错误体（限流/鉴权/模型不支持搜索等）
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("AI 请求失败 (%d): %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	// 解析 SSE：data: {...} 行，data: [DONE] 结束
	finishReason := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return finishReason, nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 忽略无法解析的心跳/空行
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return finishReason, fmt.Errorf("AI 流式错误: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) > 0 {
			if chunk.Choices[0].FinishReason != "" {
				finishReason = chunk.Choices[0].FinishReason
			}
			if chunk.Choices[0].Delta.Content != "" {
				onDelta(chunk.Choices[0].Delta.Content)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return finishReason, err
	}
	return finishReason, nil
}
