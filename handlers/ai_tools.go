// Copyright (c) VisualClassroom. All rights reserved.

// 新增 AI 教学工具（均走 qwenCreateChat：主力模型 + 一次自动重试）：
//
//	POST /api/ai/comment         班级学情 → 学生期末评语草稿（教师）
//	POST /api/ai/error-analysis  错题归因分析（师生可用）
//	POST /api/ai/lesson-summary  课堂小结（教师）
//
// 输入统一做长度限制与频率限制，防止滥用刷调用。
package handlers

import (
	"strings"

	"github.com/gofiber/fiber/v3"
)

// callQwenText 通用文本调用：系统提示词 + 用户输入，返回正文。
// 输入超过 maxRunes 时截断（保护提示词长度与调用成本）。
func callQwenText(c fiber.Ctx, systemPrompt, userInput string, maxRunes int) (string, error) {
	if !qwenAPIKeyed {
		return "", fiber.NewError(fiber.StatusServiceUnavailable, qwenErrUnavailable())
	}
	if !rateLimitAllow(c.IP()) {
		return "", fiber.NewError(fiber.StatusTooManyRequests, "操作过于频繁，请稍后再试")
	}
	input := userInput
	if r := []rune(input); len(r) > maxRunes {
		input = string(r[:maxRunes])
	}
	resp, err := qwenCreateChat(c.Context(), chatRequest(systemPrompt, input))
	if err != nil {
		return "", fiber.NewError(fiber.StatusBadGateway, "AI 调用失败："+err.Error())
	}
	if len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		return "", fiber.NewError(fiber.StatusBadGateway, "AI 未返回有效内容，请重试")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// AiStudentComment 生成学生评语草稿（教师端）。
func AiStudentComment(c fiber.Ctx) error {
	if c.Locals("role").(string) != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "仅教师可用"})
	}
	var req struct {
		StudentName string `json:"student_name"`
		Snapshot    string `json:"snapshot"` // 学业数据快照（成绩趋势/作业完成/课堂表现等自由文本）
	}
	if err := c.Bind().JSON(&req); err != nil || strings.TrimSpace(req.Snapshot) == "" {
		return c.Status(400).JSON(fiber.Map{"error": "请提供学生学业数据快照"})
	}
	userInput := "学生：" + strings.TrimSpace(req.StudentName) + "\n学业数据快照：\n" + strings.TrimSpace(req.Snapshot)
	text, err := callQwenText(c, aiCommentSystemPrompt, userInput, 4000)
	if err != nil {
		return writeQwenError(c, err)
	}
	return c.JSON(fiber.Map{"comment": text})
}

// AiErrorAnalysis 错题归因分析（师生可用：学生自查、教师备课）。
func AiErrorAnalysis(c fiber.Ctx) error {
	var req struct {
		Question      string `json:"question"`       // 题干
		StudentAnswer string `json:"student_answer"` // 学生答案
		CorrectAnswer string `json:"correct_answer"` // 正确答案
		Subject       string `json:"subject"`        // 学科（可选）
	}
	if err := c.Bind().JSON(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		return c.Status(400).JSON(fiber.Map{"error": "请提供题目内容"})
	}
	var b strings.Builder
	if s := strings.TrimSpace(req.Subject); s != "" {
		b.WriteString("学科：" + s + "\n")
	}
	b.WriteString("题干：\n" + strings.TrimSpace(req.Question) + "\n")
	b.WriteString("学生答案：\n" + strings.TrimSpace(req.StudentAnswer) + "\n")
	b.WriteString("正确答案：\n" + strings.TrimSpace(req.CorrectAnswer))
	text, err := callQwenText(c, aiErrorAnalysisSystemPrompt, b.String(), 6000)
	if err != nil {
		return writeQwenError(c, err)
	}
	return c.JSON(fiber.Map{"analysis": text})
}

// AiLessonSummary 课堂小结（教师课后复盘）。
func AiLessonSummary(c fiber.Ctx) error {
	if c.Locals("role").(string) != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "仅教师可用"})
	}
	var req struct {
		Lesson  string `json:"lesson"`  // 课题
		Summary string `json:"summary"` // 课堂互动数据快照（答题/参与/高频错题等自由文本）
	}
	if err := c.Bind().JSON(&req); err != nil || strings.TrimSpace(req.Summary) == "" {
		return c.Status(400).JSON(fiber.Map{"error": "请提供课堂互动数据"})
	}
	userInput := "课题：" + strings.TrimSpace(req.Lesson) + "\n课堂互动数据：\n" + strings.TrimSpace(req.Summary)
	text, err := callQwenText(c, lessonSummarySystemPrompt, userInput, 4000)
	if err != nil {
		return writeQwenError(c, err)
	}
	return c.JSON(fiber.Map{"summary": text})
}

// writeQwenError 把 callQwenText 的 fiber 错误原样（状态码+消息）回给前端。
func writeQwenError(c fiber.Ctx, err error) error {
	if fe, ok := err.(*fiber.Error); ok {
		return c.Status(fe.Code).JSON(fiber.Map{"error": fe.Message})
	}
	return c.Status(500).JSON(fiber.Map{"error": err.Error()})
}
