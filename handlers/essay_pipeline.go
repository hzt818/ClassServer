// Copyright (c) 黄智韬. All rights reserved.

// 教师流水线批阅 + 优秀作文展示 + 学生管理：
//   - /api/essay/pipeline      教师查看全班批改队列（流水线视图数据源）
//   - /api/essay/students      教师端学生作文统计（管理系统数据源）
//   - /api/essay/excellent     优秀作文列表（全员可看，学生自主学习）
//   - /api/essay/jobs/:id/excellent   教师标记/取消优秀作文
//   - /api/essay/jobs/:id/annotated   AI 批注图（原图+标注框）
//   - /pipeline /showcase      对应页面
package handlers

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"visualclassroom/classserver/database"

	"github.com/gofiber/fiber/v3"
)

// ---------- 页面 ----------

func ShowPipeline(c fiber.Ctx) error {
	username := c.Locals("username").(string)
	var fullName string
	_ = database.Pool.QueryRow(c.Context(), "SELECT full_name FROM users WHERE username=$1", username).Scan(&fullName)
	if fullName == "" {
		fullName = username
	}
	avatar, initial := loadAvatar(c, username, fullName)
	return c.Render("pipeline", fiber.Map{
		"Username":  username,
		"FullName":  fullName,
		"Role":      "teacher",
		"Active":    "pipeline",
		"LastLogin": "",
		"Avatar":    avatar,
		"Initial":   initial,
	})
}

func ShowShowcase(c fiber.Ctx) error {
	username := c.Locals("username").(string)
	role := c.Locals("role").(string)
	var fullName string
	_ = database.Pool.QueryRow(c.Context(), "SELECT full_name FROM users WHERE username=$1", username).Scan(&fullName)
	if fullName == "" {
		fullName = username
	}
	avatar, initial := loadAvatar(c, username, fullName)
	return c.Render("showcase", fiber.Map{
		"Username":  username,
		"FullName":  fullName,
		"Role":      role,
		"Active":    "showcase",
		"LastLogin": "",
		"Avatar":    avatar,
		"Initial":   initial,
	})
}

// ShowCast 学生端课堂传屏页。
func ShowCast(c fiber.Ctx) error {
	username := c.Locals("username").(string)
	role := c.Locals("role").(string)
	var fullName string
	_ = database.Pool.QueryRow(c.Context(), "SELECT full_name FROM users WHERE username=$1", username).Scan(&fullName)
	if fullName == "" {
		fullName = username
	}
	avatar, initial := loadAvatar(c, username, fullName)
	return c.Render("cast", fiber.Map{
		"Username":  username,
		"FullName":  fullName,
		"Role":      role,
		"Active":    "cast",
		"LastLogin": "",
		"Avatar":    avatar,
		"Initial":   initial,
	})
}

// ---------- 教师流水线：全班批改队列 ----------

type pipelineJob struct {
	ID             int    `json:"id"`
	StudentName    string `json:"student_name"`
	Username       string `json:"username"`
	Title          string `json:"title"`
	Type           string `json:"type"`
	Status         string `json:"status"`
	ScoreText      string `json:"score_text"`
	MaxScore       int    `json:"max_score"`
	TeacherComment string `json:"teacher_comment"`
	ShortComment   string `json:"short_comment"`
	AnnotatedPath  string `json:"annotated_path"`
	Excellent      bool   `json:"excellent"`
	HasImage       bool   `json:"has_image"`
	TeacherEdited  bool   `json:"teacher_edited"`
}

// GetPipelineJobs 教师拉取全班（所有用户）最近 300 条未删除批改任务。
func GetPipelineJobs(c fiber.Ctx) error {
	rows, err := database.Pool.Query(c.Context(),
		`SELECT id, COALESCE(student_name,''), username, COALESCE(title,''), type, status,
		        COALESCE(score_text,''), max_score, COALESCE(teacher_comment,''),
		        COALESCE(short_comment,''), COALESCE(annotated_path,''), excellent, COALESCE(image_path,'') <> '', teacher_edited
		 FROM essay_submissions WHERE is_deleted=false ORDER BY id DESC LIMIT 300`)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()
	jobs := make([]pipelineJob, 0)
	for rows.Next() {
		var j pipelineJob
		if err := rows.Scan(&j.ID, &j.StudentName, &j.Username, &j.Title, &j.Type, &j.Status,
			&j.ScoreText, &j.MaxScore, &j.TeacherComment, &j.ShortComment, &j.AnnotatedPath, &j.Excellent,
			&j.HasImage, &j.TeacherEdited); err == nil {
			jobs = append(jobs, j)
		}
	}
	return c.JSON(jobs)
}

// ---------- 学生管理：按学生聚合作文统计 ----------

type studentStat struct {
	UserID        int      `json:"user_id"`
	Username      string   `json:"username"`
	FullName      string   `json:"full_name"`
	EssayCount    int      `json:"essay_count"`
	AvgScore      *float64 `json:"avg_score"` // 归一化到百分制
	ExcellentCnt  int      `json:"excellent_count"`
	LastSubmitted string   `json:"last_submitted"`
}

// GetStudentStats 教师端学生作文统计：篇数 / 百分制均分 / 优秀篇数 / 最近提交时间。
func GetStudentStats(c fiber.Ctx) error {
	rows, err := database.Pool.Query(c.Context(),
		`SELECT u.id, u.username, COALESCE(u.full_name,''),
		        COALESCE(COUNT(s.id) FILTER (WHERE s.is_deleted=false), 0),
		        COALESCE(AVG(CAST(s.score_text AS REAL) / s.max_score * 100) FILTER (
		            WHERE s.is_deleted=false AND s.status='done' AND s.score_text IS NOT NULL
		                  AND s.score_text <> '' AND s.score_text <> '--' AND s.score_text <> '错误'), NULL),
		        COALESCE(COUNT(s.id) FILTER (WHERE s.is_deleted=false AND s.excellent), 0),
		        COALESCE(MAX(s.created_at) FILTER (WHERE s.is_deleted=false), '')
		 FROM users u LEFT JOIN essay_submissions s ON s.user_id = u.id
		 WHERE u.role='student'
		 GROUP BY u.id, u.username, u.full_name
		 ORDER BY u.id`)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()
	stats := make([]studentStat, 0)
	for rows.Next() {
		var s studentStat
		var last string
		if err := rows.Scan(&s.UserID, &s.Username, &s.FullName, &s.EssayCount, &s.AvgScore, &s.ExcellentCnt, &last); err == nil {
			s.LastSubmitted = last
			stats = append(stats, s)
		}
	}
	return c.JSON(stats)
}

// ---------- 优秀作文 ----------

// ToggleEssayExcellent 教师标记/取消优秀作文。
func ToggleEssayExcellent(c fiber.Ctx) error {
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	var req struct {
		Excellent *bool `json:"excellent"`
	}
	_ = c.Bind().JSON(&req)
	var excellent bool
	if req.Excellent != nil {
		excellent = *req.Excellent
	} else {
		// 未指定时按当前状态取反
		if err := database.Pool.QueryRow(c.Context(),
			"SELECT COALESCE(excellent,false) FROM essay_submissions WHERE id=$1", id).Scan(&excellent); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "任务不存在"})
		}
		excellent = !excellent
	}
	res, err := database.Pool.Exec(c.Context(),
		"UPDATE essay_submissions SET excellent=$1, updated_at=CURRENT_TIMESTAMP WHERE id=$2 AND is_deleted=false", excellent, id)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	if rows := res.RowsAffected(); rows == 0 {
		return c.Status(404).JSON(fiber.Map{"error": "任务不存在"})
	}
	return c.JSON(fiber.Map{"ok": true, "excellent": excellent})
}

// SetEssayShortComment 教师逐篇短评（可留空清除）。请求体 {"comment": "..."}。
func SetEssayShortComment(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "仅教师可写短评"})
	}
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	var req struct {
		Comment string `json:"comment"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "参数错误"})
	}
	comment := strings.TrimSpace(req.Comment)
	if len([]rune(comment)) > 300 {
		comment = string([]rune(comment)[:300])
	}
	res, err := database.Pool.Exec(c.Context(),
		"UPDATE essay_submissions SET short_comment=$1, updated_at=CURRENT_TIMESTAMP WHERE id=$2 AND is_deleted=false",
		comment, id)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	if rows := res.RowsAffected(); rows == 0 {
		return c.Status(404).JSON(fiber.Map{"error": "任务不存在"})
	}
	return c.JSON(fiber.Map{"ok": true, "comment": comment})
}

// RegradeEssayJob 教师对旧记录发起重新批改：按原内容/类型/满分/原图
// 新建一条批改任务（旧记录保留可对照），用于给早期批改的作文补齐
// 升格指导、全文升格范文等新章节并应用新的评分校准。
func RegradeEssayJob(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "仅教师可重新批改"})
	}
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	var owner int
	var username, title, content, typ, studentName, imagePath string
	var maxScore int
	err = database.Pool.QueryRow(c.Context(),
		`SELECT user_id, username, COALESCE(title,''), content, type, COALESCE(student_name,''), max_score, COALESCE(image_path,'')
		 FROM essay_submissions WHERE id=$1 AND is_deleted=false`, id).
		Scan(&owner, &username, &title, &content, &typ, &studentName, &maxScore, &imagePath)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "任务不存在"})
	}
	if strings.TrimSpace(content) == "" {
		return c.Status(400).JSON(fiber.Map{"error": "该记录没有作文内容，无法重新批改"})
	}
	var jobID int
	err = database.Pool.QueryRow(c.Context(),
		"INSERT INTO essay_submissions (user_id, username, title, content, type, student_name, max_score, status, is_streaming, feedback, image_path) VALUES ($1, $2, $3, $4, $5, $6, $7, 'processing', true, '', $8) RETURNING id",
		owner, username, title, content, typ, studentName, maxScore, nullIfEmpty(imagePath)).Scan(&jobID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "创建批改任务失败: " + err.Error()})
	}
	registerEssayStream(jobID)
	go runEssayGrading(jobID, typ, content, imagePath, 0.5, maxScore)
	return c.JSON(fiber.Map{"ok": true, "id": jobID, "status": "processing"})
}

// ListExcellentEssays 优秀作文列表（全员可访问，供学生查看学习）。
func ListExcellentEssays(c fiber.Ctx) error {
	rows, err := database.Pool.Query(c.Context(),
		`SELECT id, COALESCE(student_name,''), username, COALESCE(title,''), type, content,
		        COALESCE(feedback,''), COALESCE(teacher_comment,''), COALESCE(score_text,''),
		        max_score, COALESCE(image_path,''), COALESCE(annotated_path,''), COALESCE(annotations,''), created_at
		 FROM essay_submissions
		 WHERE is_deleted=false AND excellent=true AND status='done'
		 ORDER BY updated_at DESC LIMIT 100`)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()
	list := make([]EssayJob, 0)
	for rows.Next() {
		var j EssayJob
		if err := rows.Scan(&j.ID, &j.StudentName, &j.Username, &j.Title, &j.Type, &j.Content,
			&j.Feedback, &j.TeacherComment, &j.ScoreText, &j.MaxScore, &j.ImagePath,
			&j.AnnotatedPath, &j.Annotations, &j.CreatedAt); err == nil {
			list = append(list, j)
		}
	}
	return c.JSON(list)
}

// ---------- AI 批注图 ----------

// GetEssayJobAnnotatedImage 返回某批改任务的 AI 批注图（原图 + 标注框）。
func GetEssayJobAnnotatedImage(c fiber.Ctx) error {
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	userID := c.Locals("userID").(int)
	role := c.Locals("role").(string)

	var owner int
	var imagePath string
	err = database.Pool.QueryRow(c.Context(),
		"SELECT user_id, COALESCE(annotated_path,'') FROM essay_submissions WHERE id=$1", id,
	).Scan(&owner, &imagePath)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "任务不存在"})
	}
	if owner != userID && role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权访问"})
	}
	if imagePath == "" {
		return c.Status(404).JSON(fiber.Map{"error": "批注图尚未生成（AI 正在处理或无原图）"})
	}

	fullPath := filepath.Join(annotatedImageDir, filepath.Base(imagePath))
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return c.Status(404).JSON(fiber.Map{"error": "批注图文件丢失"})
	}
	return c.SendFile(fullPath)
}

// GetEssayAnnotations 返回某批改任务的批注列表 JSON（前端弹窗展示编号对应批注文字）。
func GetEssayAnnotations(c fiber.Ctx) error {
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	userID := c.Locals("userID").(int)
	role := c.Locals("role").(string)
	var owner int
	var annJSON string
	err = database.Pool.QueryRow(c.Context(),
		"SELECT user_id, COALESCE(annotations,'') FROM essay_submissions WHERE id=$1", id,
	).Scan(&owner, &annJSON)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "任务不存在"})
	}
	if owner != userID && role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权访问"})
	}
	if annJSON == "" {
		annJSON = "[]"
	}
	return c.SendString(annJSON)
}
