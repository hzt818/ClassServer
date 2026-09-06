package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"visualclassroom/classserver/config"
	"visualclassroom/classserver/database"

	"github.com/gofiber/fiber/v3"
	"golang.org/x/crypto/bcrypt"
)

// ---------- 数据结构 ----------
type UserInfo struct {
	ID        int        `json:"id"`
	Username  string     `json:"username"`
	Role      string     `json:"role"`
	FullName  string     `json:"full_name"`
	Avatar    string     `json:"avatar"`
	LastLogin *time.Time `json:"last_login"`
	CreatedAt time.Time  `json:"created_at"`
}

// ---------- 页面 ----------
func ShowAdminUsers(c fiber.Ctx) error {
	username := c.Locals("username").(string)
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).SendString("无权限")
	}
	var fullName string
	_ = database.Pool.QueryRow(c.Context(), "SELECT full_name FROM users WHERE username=$1", username).Scan(&fullName)
	if fullName == "" {
		fullName = username
	}
	userID := c.Locals("userID").(int)
	cfg := config.LoadConfig()
	avatar, initial := loadAvatar(c, username, fullName)
	return c.Render("admin_users", fiber.Map{
		"Username":  username,
		"FullName":  fullName,
		"Role":      role,
		"Active":    "admin_users",
		"LastLogin": time.Now().Format("2006-01-02 15:04"),
		"UserID":    userID,
		"BuildDev":  cfg.BuildDev, // 控制"导出记录"等开发/测试期功能按钮是否显示
		"Avatar":    avatar,
		"Initial":   initial,
	})
}

// ---------- API ----------
func GetUsers(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}
	rows, err := database.Pool.Query(c.Context(),
		"SELECT id, username, role, full_name, avatar, last_login, created_at FROM users ORDER BY id")
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()
	var users []UserInfo
	for rows.Next() {
		var u UserInfo
		var avatar sql.NullString
		err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.FullName, &avatar, &u.LastLogin, &u.CreatedAt)
		if err != nil {
			continue
		}
		if avatar.Valid {
			u.Avatar = avatar.String
		} else {
			u.Avatar = ""
		}
		users = append(users, u)
	}
	return c.JSON(users)
}

// createUserIfAbsent 新建单个用户；已存在或写入失败返回 false（逐条 best-effort）。
func createUserIfAbsent(ctx context.Context, username, role string) bool {
	var exists bool
	if err := database.Pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM users WHERE username=$1)", username).Scan(&exists); err != nil || exists {
		return false
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(username), bcrypt.DefaultCost)
	if err != nil {
		return false
	}
	if _, err := database.Pool.Exec(ctx,
		"INSERT INTO users (username, password_hash, role, full_name) VALUES ($1, $2, $3, $4)",
		username, string(hashed), role, username,
	); err != nil {
		return false
	}
	return true
}

func CreateUsers(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}
	var req []struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "请求格式错误"})
	}
	if len(req) == 0 {
		return c.JSON(fiber.Map{"success": true, "count": 0})
	}
	inserted := 0
	for _, item := range req {
		itemRole := item.Role
		if itemRole == "" {
			itemRole = "student"
		}
		if createUserIfAbsent(c.Context(), item.Username, itemRole) {
			inserted++
		}
	}
	return c.JSON(fiber.Map{"success": true, "count": inserted})
}

// UpdateUser 支持部分更新，使用指针字段区分"未传递"和"传递空值"。
// 全部字段走一条静态 SQL：未传递的列用 COALESCE 保持原值，避免动态拼接 SET 子句。
func UpdateUser(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}
	idStr := c.Params("id")
	if idStr == "" {
		return c.Status(400).JSON(fiber.Map{"error": "缺少用户ID"})
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}

	var req struct {
		Username *string `json:"username"`
		Role     *string `json:"role"`
		FullName *string `json:"full_name"`
		Password *string `json:"password"`
		Avatar   *string `json:"avatar"` // 指针：传递 null 表示不更新，传递 "" 表示清除
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "请求格式错误"})
	}

	if req.Username != nil && *req.Username == "" {
		return c.Status(400).JSON(fiber.Map{"error": "用户名不能为空"})
	}
	if req.Role != nil && *req.Role == "" {
		return c.Status(400).JSON(fiber.Map{"error": "角色不能为空"})
	}
	if req.Username == nil && req.Role == nil && req.FullName == nil && req.Password == nil && req.Avatar == nil {
		return c.Status(400).JSON(fiber.Map{"error": "没有要更新的字段"})
	}

	var passwordHash *string
	if req.Password != nil && *req.Password != "" {
		hashed, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "密码加密失败"})
		}
		passwordHash = new(string)
		*passwordHash = string(hashed)
	}

	_, err = database.Pool.Exec(c.Context(),
		"UPDATE users SET username = COALESCE($1, username), role = COALESCE($2, role), full_name = COALESCE($3, full_name), password_hash = COALESCE($4, password_hash), avatar = COALESCE($5, avatar) WHERE id = $6",
		req.Username, req.Role, req.FullName, passwordHash, req.Avatar, id,
	)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}

func DeleteUsers(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}
	var req struct {
		IDs []int `json:"ids"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "请求格式错误"})
	}
	if len(req.IDs) == 0 {
		return c.JSON(fiber.Map{"success": true, "count": 0})
	}
	currentUserID := c.Locals("userID").(int)
	var filtered []int
	for _, id := range req.IDs {
		if id != currentUserID {
			filtered = append(filtered, id)
		}
	}
	if len(filtered) == 0 {
		return c.Status(400).JSON(fiber.Map{"error": "不能删除自己的账号"})
	}
	// ANY($1) 由数据库适配层展开为 IN (?,?,...)
	tag, err := database.Pool.Exec(c.Context(), "DELETE FROM users WHERE id = ANY($1)", filtered)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true, "count": int(tag.RowsAffected())})
}

func DeleteUser(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}
	idStr := c.Params("id")
	if idStr == "" {
		return c.Status(400).JSON(fiber.Map{"error": "缺少用户ID"})
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	currentUserID := c.Locals("userID").(int)
	if id == currentUserID {
		return c.Status(400).JSON(fiber.Map{"error": "不能删除自己的账号"})
	}
	_, err = database.Pool.Exec(c.Context(), "DELETE FROM users WHERE id=$1", id)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}

// renameOneUser 重命名单个用户；目标不可用或冲突返回 false（逐条 best-effort）。
func renameOneUser(ctx context.Context, oldName, newName string) bool {
	var exists bool
	if err := database.Pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM users WHERE username=$1)", oldName).Scan(&exists); err != nil || !exists {
		return false
	}
	var used bool
	if err := database.Pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM users WHERE username=$1 AND username!=$2)", newName, oldName).Scan(&used); err != nil || used {
		return false
	}
	if _, err := database.Pool.Exec(ctx,
		"UPDATE users SET username=$1 WHERE username=$2", newName, oldName); err != nil {
		return false
	}
	return true
}

func RenameUsers(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}
	var req struct {
		Mappings map[string]string `json:"mappings"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "请求格式错误"})
	}
	if len(req.Mappings) == 0 {
		return c.JSON(fiber.Map{"success": true, "count": 0})
	}
	updated := 0
	for oldName, newName := range req.Mappings {
		if oldName == "" || newName == "" {
			continue
		}
		if renameOneUser(c.Context(), oldName, newName) {
			updated++
		}
	}
	return c.JSON(fiber.Map{"success": true, "count": updated})
}

// UploadAvatar 上传头像
func UploadAvatar(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}
	idStr := c.Params("id")
	if idStr == "" {
		return c.Status(400).JSON(fiber.Map{"error": "缺少用户ID"})
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	file, err := c.FormFile("avatar")
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "请上传图片"})
	}
	if file.Size > 2*1024*1024 {
		return c.Status(400).JSON(fiber.Map{"error": "图片大小不能超过2MB"})
	}
	ext := filepath.Ext(file.Filename)
	if ext == "" {
		ext = ".png"
	}
	filename := fmt.Sprintf("avatar_%d_%d%s", id, time.Now().Unix(), ext)
	savePath := filepath.Join("./static/uploads/avatars", filename)
	if err := os.MkdirAll(filepath.Dir(savePath), 0755); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "保存失败"})
	}
	if err := c.SaveFile(file, savePath); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "保存失败"})
	}
	avatarURL := "/static/uploads/avatars/" + filename
	_, err = database.Pool.Exec(c.Context(), "UPDATE users SET avatar=$1 WHERE id=$2", avatarURL, id)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true, "avatar": avatarURL})
}

// ---------- 聊天时间管理 ----------
// 定义接收结构体
type ChatTimeRuleInput struct {
	ID           int    `json:"id"`
	Mode         string `json:"mode"`
	DayOfWeek    *int   `json:"day_of_week"`
	SpecificDate string `json:"specific_date"` // "YYYY-MM-DD"
	StartTime    string `json:"start_time"`    // "HH:MM"
	EndTime      string `json:"end_time"`      // "HH:MM"
}

// 数据库存储结构（SQLite 中 specific_date/start_time/end_time 均为 TEXT 列）
type ChatTimeRule struct {
	ID           int        `json:"id"`
	Mode         string     `json:"mode"`
	DayOfWeek    *int       `json:"day_of_week,omitempty"`
	SpecificDate *time.Time `json:"specific_date,omitempty"`
	StartTime    string     `json:"start_time"`
	EndTime      string     `json:"end_time"`
}

// GetChatTimeRules 教师获取规则
func GetChatTimeRules(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}
	rules, err := fetchChatTimeRules(c.Context())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(rules)
}

// GetChatTimeRulesPublic 公开获取规则（学生）
func GetChatTimeRulesPublic(c fiber.Ctx) error {
	rules, err := fetchChatTimeRules(c.Context())
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(rules)
}

// fetchChatTimeRules 内部共用函数。
// SQLite 中这三列声明为 TEXT：specific_date 为 "YYYY-MM-DD"，start/end 为 "HH:MM"。
func fetchChatTimeRules(ctx context.Context) ([]ChatTimeRule, error) {
	rows, err := database.Pool.Query(ctx,
		"SELECT id, mode, day_of_week, specific_date, start_time, end_time FROM chat_time_rules ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []ChatTimeRule
	for rows.Next() {
		var r ChatTimeRule
		var dayOfWeek sql.NullInt32
		var specificDate sql.NullString
		var startTime, endTime string
		err := rows.Scan(&r.ID, &r.Mode, &dayOfWeek, &specificDate, &startTime, &endTime)
		if err != nil {
			continue
		}
		if dayOfWeek.Valid {
			val := int(dayOfWeek.Int32)
			r.DayOfWeek = &val
		}
		if specificDate.Valid && specificDate.String != "" {
			if t, perr := time.Parse("2006-01-02", specificDate.String); perr == nil {
				r.SpecificDate = &t
			}
		}
		r.StartTime = startTime
		r.EndTime = endTime
		rules = append(rules, r)
	}
	if rules == nil {
		rules = []ChatTimeRule{}
	}
	return rules, nil
}

// chatTimeRuleValid 校验单条规则（供 SetChatTimeRules 使用）。
func chatTimeRuleValid(rule ChatTimeRuleInput, mode string) error {
	if rule.Mode != mode {
		return fmt.Errorf("所有规则模式必须一致")
	}
	if mode == "weekday" {
		if rule.DayOfWeek == nil || *rule.DayOfWeek < 1 || *rule.DayOfWeek > 7 {
			return fmt.Errorf("星期模式必须指定有效 day_of_week (1-7)")
		}
	} else {
		if rule.SpecificDate == "" {
			return fmt.Errorf("日期模式必须指定 specific_date")
		}
		if _, err := time.Parse("2006-01-02", rule.SpecificDate); err != nil {
			return fmt.Errorf("日期格式错误，需 YYYY-MM-DD")
		}
	}
	if rule.StartTime == "" || rule.EndTime == "" {
		return fmt.Errorf("请填写起止时间")
	}
	if _, err := time.Parse("15:04", rule.StartTime); err != nil {
		return fmt.Errorf("开始时间格式错误，需 HH:MM")
	}
	if _, err := time.Parse("15:04", rule.EndTime); err != nil {
		return fmt.Errorf("结束时间格式错误，需 HH:MM")
	}
	if rule.StartTime >= rule.EndTime {
		return fmt.Errorf("开始时间必须早于结束时间")
	}
	return nil
}

// insertChatTimeRule 写入单条规则。start/end/specific_date 为 TEXT 列，直接存字符串。
func insertChatTimeRule(ctx context.Context, mode string, dayOfWeek *int, specificDate *string, startTime, endTime string) error {
	_, err := database.Pool.Exec(ctx,
		"INSERT INTO chat_time_rules (mode, day_of_week, specific_date, start_time, end_time) VALUES ($1, $2, $3, $4, $5)",
		mode, dayOfWeek, specificDate, startTime, endTime)
	return err
}

// SetChatTimeRules 替换所有规则。SQLite 单连接写入，逐条 best-effort（原事务语义由校验前置保证）。
func SetChatTimeRules(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}
	var req struct {
		Rules []ChatTimeRuleInput `json:"rules"`
		Mode  string              `json:"mode"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "请求格式错误: " + err.Error()})
	}
	mode := req.Mode
	if len(req.Rules) == 0 {
		if _, err := database.Pool.Exec(c.Context(), "DELETE FROM chat_time_rules"); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"success": true})
	}
	if mode != "weekday" && mode != "date" {
		return c.Status(400).JSON(fiber.Map{"error": "模式必须为 weekday 或 date"})
	}
	// 先完整校验，再清空重写，保证不会写入半套非法规则
	for _, rule := range req.Rules {
		if err := chatTimeRuleValid(rule, mode); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": err.Error()})
		}
	}
	if _, err := database.Pool.Exec(c.Context(), "DELETE FROM chat_time_rules"); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	for _, rule := range req.Rules {
		var dayOfWeek *int
		var specificDate *string
		if mode == "weekday" {
			dayOfWeek = rule.DayOfWeek
		} else {
			specificDate = &rule.SpecificDate
		}
		if err := insertChatTimeRule(c.Context(), mode, dayOfWeek, specificDate, rule.StartTime, rule.EndTime); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
	}
	return c.JSON(fiber.Map{"success": true})
}

// ---------- 需求二：用户操作记录导出 ----------
// ExportRecords 导出聊天记录 / 批改记录（教师专用），CSV 格式，可选按用户筛选。
// GET /api/admin/export/records?type=all|chat|essay&user_ids=1,2,3
func ExportRecords(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}

	recordType := c.Query("type", "all")
	if recordType != "all" && recordType != "chat" && recordType != "essay" {
		return c.Status(400).JSON(fiber.Map{"error": "无效的记录类型"})
	}

	var userIDs []int
	if idsStr := c.Query("user_ids", ""); idsStr != "" {
		for _, part := range strings.Split(idsStr, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.Atoi(part)
			if err != nil {
				return c.Status(400).JSON(fiber.Map{"error": "无效的用户ID: " + part})
			}
			userIDs = append(userIDs, id)
		}
	}

	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // UTF-8 BOM，避免 Excel 打开中文乱码
	w := csv.NewWriter(&buf)

	if recordType == "chat" || recordType == "all" {
		if err := exportChatRecordsCSV(c.Context(), w, userIDs); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "导出聊天记录失败: " + err.Error()})
		}
	}
	if recordType == "essay" || recordType == "all" {
		if recordType == "all" {
			_ = w.Write([]string{}) // 空行分隔两段记录，便于在 Excel 中区分
		}
		if err := exportEssayRecordsCSV(c.Context(), w, userIDs); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "导出批改记录失败: " + err.Error()})
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}

	filename := fmt.Sprintf("records_%s.csv", time.Now().Format("20060102_150405"))
	c.Set("Content-Type", "text/csv; charset=utf-8")
	c.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	return c.Send(buf.Bytes())
}

func exportChatRecordsCSV(ctx context.Context, w *csv.Writer, userIDs []int) error {
	_ = w.Write([]string{"【聊天记录】"})
	_ = w.Write([]string{"用户名", "房间", "内容", "消息类型", "发送时间", "是否已撤回"})

	// 有无筛选走两条完整静态 SQL，避免拼接查询文本
	var rows *database.Rows
	var err error
	if len(userIDs) > 0 {
		rows, err = database.Pool.Query(ctx,
			"SELECT m.username, r.name, m.content, m.msg_type, m.created_at, m.is_recalled FROM messages m JOIN chat_rooms r ON r.id = m.room_id WHERE m.user_id = ANY($1) ORDER BY m.created_at ASC",
			userIDs)
	} else {
		rows, err = database.Pool.Query(ctx,
			"SELECT m.username, r.name, m.content, m.msg_type, m.created_at, m.is_recalled FROM messages m JOIN chat_rooms r ON r.id = m.room_id ORDER BY m.created_at ASC")
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var username, roomName, content, msgType string
		var createdAt time.Time
		var isRecalled bool
		if err := rows.Scan(&username, &roomName, &content, &msgType, &createdAt, &isRecalled); err != nil {
			continue
		}
		recalledText := ""
		if isRecalled {
			recalledText = "是"
		}
		_ = w.Write([]string{username, roomName, content, msgType, createdAt.Format("2006-01-02 15:04:05"), recalledText})
	}
	return rows.Err()
}

func exportEssayRecordsCSV(ctx context.Context, w *csv.Writer, userIDs []int) error {
	_ = w.Write([]string{"【作文批改记录】"})
	_ = w.Write([]string{"用户名", "学生姓名", "作文类型", "题目", "得分", "满分", "状态", "提交时间"})

	var rows *database.Rows
	var err error
	if len(userIDs) > 0 {
		rows, err = database.Pool.Query(ctx,
			"SELECT username, COALESCE(student_name,''), type, COALESCE(title,''), COALESCE(score_text,''), max_score, status, created_at FROM essay_submissions WHERE is_deleted = false AND user_id = ANY($1) ORDER BY created_at ASC",
			userIDs)
	} else {
		rows, err = database.Pool.Query(ctx,
			"SELECT username, COALESCE(student_name,''), type, COALESCE(title,''), COALESCE(score_text,''), max_score, status, created_at FROM essay_submissions WHERE is_deleted = false ORDER BY created_at ASC")
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var username, studentName, essayType, title, scoreText, status string
		var maxScore int
		var createdAt time.Time
		if err := rows.Scan(&username, &studentName, &essayType, &title, &scoreText, &maxScore, &status, &createdAt); err != nil {
			continue
		}
		_ = w.Write([]string{username, studentName, essayType, title, scoreText, strconv.Itoa(maxScore), status, createdAt.Format("2006-01-02 15:04:05")})
	}
	return rows.Err()
}
