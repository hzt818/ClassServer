package handlers

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"visualclassroom/classserver/database"
	"visualclassroom/classserver/models"

	"github.com/gofiber/fiber/v3"
)

const uploadDir = "./static/uploads/files"

func init() {
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Printf("创建文件上传目录失败: %v", err)
	}
}

// UploadFile 上传文件
func UploadFile(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	_ = c.Locals("username").(string)
	role := c.Locals("role").(string)

	roomIDStr := c.FormValue("room_id")
	if roomIDStr == "" {
		return c.Status(400).JSON(fiber.Map{"error": "缺少房间ID"})
	}
	roomID, err := strconv.Atoi(roomIDStr)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效的房间ID"})
	}

	if role != "teacher" {
		var visible bool
		err := database.Pool.QueryRow(c.Context(),
			`SELECT EXISTS(
				SELECT 1 FROM chat_rooms r
				LEFT JOIN user_hidden_rooms h ON h.room_id = r.id AND h.user_id = $1
				WHERE r.id = $2 AND r.is_deleted = false AND h.room_id IS NULL
			)`, userID, roomID,
		).Scan(&visible)
		if err != nil || !visible {
			return c.Status(403).JSON(fiber.Map{"error": "无权访问该房间"})
		}
	} else {
		var exists bool
		err := database.Pool.QueryRow(c.Context(), "SELECT EXISTS(SELECT 1 FROM chat_rooms WHERE id=$1 AND is_deleted=false)", roomID).Scan(&exists)
		if err != nil || !exists {
			return c.Status(404).JSON(fiber.Map{"error": "房间不存在"})
		}
	}

	file, err := c.FormFile("file")
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "请选择文件"})
	}
	if file.Size > 50*1024*1024 {
		return c.Status(400).JSON(fiber.Map{"error": "文件大小不能超过50MB"})
	}

	ext := filepath.Ext(file.Filename)
	if ext == "" {
		ext = ".bin"
	}
	filename := fmt.Sprintf("%d_%d_%s", userID, time.Now().UnixNano(), filepath.Base(file.Filename))
	savePath := filepath.Join(uploadDir, filename)

	if err := c.SaveFile(file, savePath); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "保存文件失败: " + err.Error()})
	}

	var fileID int
	err = database.Pool.QueryRow(c.Context(),
		`INSERT INTO chat_files (uploader_id, room_id, filename, filepath, filesize, access_type)
		 VALUES ($1, $2, $3, $4, $5, 'public') RETURNING id`,
		userID, roomID, file.Filename, "/static/uploads/files/"+filename, file.Size,
	).Scan(&fileID)
	if err != nil {
		os.Remove(savePath)
		return c.Status(500).JSON(fiber.Map{"error": "数据库写入失败: " + err.Error()})
	}

	return c.JSON(fiber.Map{
		"success":  true,
		"file_id":  fileID,
		"filename": file.Filename,
		"filesize": file.Size,
	})
}

// GetFiles 获取文件列表
func GetFiles(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	role := c.Locals("role").(string)

	roomIDStr := c.Params("roomId")
	if roomIDStr == "" {
		return c.Status(400).JSON(fiber.Map{"error": "缺少房间ID"})
	}
	roomID, err := strconv.Atoi(roomIDStr)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效的房间ID"})
	}

	var roomExists bool
	err = database.Pool.QueryRow(c.Context(), "SELECT EXISTS(SELECT 1 FROM chat_rooms WHERE id=$1 AND is_deleted=false)", roomID).Scan(&roomExists)
	if err != nil || !roomExists {
		return c.Status(404).JSON(fiber.Map{"error": "房间不存在"})
	}

	query := `
		SELECT f.id, f.uploader_id, u.username, u.role, f.room_id, f.filename, f.filepath, f.filesize,
		       f.access_type, f.target_user_id, f.is_deleted, f.uploaded_at
		FROM chat_files f
		JOIN users u ON u.id = f.uploader_id
		WHERE f.room_id = $1
	`
	args := []interface{}{roomID}
	if role != "teacher" {
		query += ` AND f.is_deleted = false
		           AND (
		               f.access_type = 'public'
		               OR (f.access_type = 'group' AND f.room_id = $1)
		               OR (f.access_type = 'private' AND f.target_user_id = $2)
		           )`
		args = append(args, userID)
	}
	query += ` ORDER BY f.uploaded_at DESC`

	rows, err := database.Pool.Query(c.Context(), query, args...)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()

	var files []models.ChatFile
	for rows.Next() {
		var f models.ChatFile
		var targetUserID sql.NullInt32
		var uploaderName, uploaderRole string
		err := rows.Scan(&f.ID, &f.UploaderID, &uploaderName, &uploaderRole, &f.RoomID,
			&f.Filename, &f.Filepath, &f.Filesize, &f.AccessType, &targetUserID, &f.IsDeleted, &f.UploadedAt)
		if err != nil {
			continue
		}
		f.UploaderName = uploaderName
		f.UploaderRole = uploaderRole
		if targetUserID.Valid {
			id := int(targetUserID.Int32)
			f.TargetUserID = &id
		} else {
			f.TargetUserID = nil
		}
		files = append(files, f)
	}
	return c.JSON(files)
}

// DownloadFile 下载文件
func DownloadFile(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	role := c.Locals("role").(string)

	fileIDStr := c.Params("fileId")
	if fileIDStr == "" {
		return c.Status(400).JSON(fiber.Map{"error": "缺少文件ID"})
	}
	fileID, err := strconv.Atoi(fileIDStr)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效的文件ID"})
	}

	var f models.ChatFile
	var targetUserID sql.NullInt32
	var uploaderName, uploaderRole string
	err = database.Pool.QueryRow(c.Context(),
		`SELECT f.id, f.uploader_id, u.username, u.role, f.room_id, f.filename, f.filepath, f.filesize,
		        f.access_type, f.target_user_id, f.is_deleted, f.uploaded_at
		 FROM chat_files f
		 JOIN users u ON u.id = f.uploader_id
		 WHERE f.id = $1`,
		fileID,
	).Scan(&f.ID, &f.UploaderID, &uploaderName, &uploaderRole, &f.RoomID,
		&f.Filename, &f.Filepath, &f.Filesize, &f.AccessType, &targetUserID, &f.IsDeleted, &f.UploadedAt)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "文件不存在"})
	}
	f.UploaderName = uploaderName
	f.UploaderRole = uploaderRole
	if targetUserID.Valid {
		id := int(targetUserID.Int32)
		f.TargetUserID = &id
	}

	if role != "teacher" {
		if f.IsDeleted {
			return c.Status(403).JSON(fiber.Map{"error": "文件已被删除"})
		}
		if f.AccessType == "public" {
			// 允许
		} else if f.AccessType == "group" && f.RoomID == f.RoomID {
			// 允许
		} else if f.AccessType == "private" && f.TargetUserID != nil && *f.TargetUserID == userID {
			// 允许
		} else {
			return c.Status(403).JSON(fiber.Map{"error": "无权访问此文件"})
		}
	}

	filePath := strings.TrimPrefix(f.Filepath, "/static/uploads/files/")
	fullPath := filepath.Join(uploadDir, filePath)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return c.Status(404).JSON(fiber.Map{"error": "物理文件丢失"})
	}

	return c.Download(fullPath, f.Filename)
}

// UpdateFileAccess 更新权限（仅教师）
func UpdateFileAccess(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "只有教师可以设置访问权限"})
	}

	fileIDStr := c.Params("fileId")
	if fileIDStr == "" {
		return c.Status(400).JSON(fiber.Map{"error": "缺少文件ID"})
	}
	fileID, err := strconv.Atoi(fileIDStr)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效的文件ID"})
	}

	var req struct {
		AccessType   string `json:"access_type"`
		TargetUserID *int   `json:"target_user_id"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "请求格式错误"})
	}
	if req.AccessType != "public" && req.AccessType != "private" && req.AccessType != "group" {
		return c.Status(400).JSON(fiber.Map{"error": "无效的访问类型"})
	}

	var uploaderRole string
	err = database.Pool.QueryRow(c.Context(),
		`SELECT u.role FROM chat_files f JOIN users u ON u.id = f.uploader_id WHERE f.id = $1`,
		fileID,
	).Scan(&uploaderRole)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "文件不存在"})
	}
	if uploaderRole == "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "不能修改其他教师上传的文件权限"})
	}

	_, err = database.Pool.Exec(c.Context(),
		`UPDATE chat_files SET access_type = $1, target_user_id = $2 WHERE id = $3`,
		req.AccessType, req.TargetUserID, fileID,
	)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}

// DeleteFile 软删除
func DeleteFile(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	role := c.Locals("role").(string)

	fileIDStr := c.Params("fileId")
	if fileIDStr == "" {
		return c.Status(400).JSON(fiber.Map{"error": "缺少文件ID"})
	}
	fileID, err := strconv.Atoi(fileIDStr)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效的文件ID"})
	}

	var uploaderID int
	var uploaderRole string
	err = database.Pool.QueryRow(c.Context(),
		`SELECT f.uploader_id, u.role FROM chat_files f JOIN users u ON u.id = f.uploader_id WHERE f.id = $1`,
		fileID,
	).Scan(&uploaderID, &uploaderRole)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "文件不存在"})
	}

	if role == "teacher" {
		if uploaderRole == "teacher" && uploaderID != userID {
			return c.Status(403).JSON(fiber.Map{"error": "不能删除其他教师上传的文件"})
		}
	} else {
		if uploaderID != userID {
			return c.Status(403).JSON(fiber.Map{"error": "只能删除自己上传的文件"})
		}
	}

	_, err = database.Pool.Exec(c.Context(),
		`UPDATE chat_files SET is_deleted = true, deleted_at = CURRENT_TIMESTAMP WHERE id = $1`,
		fileID,
	)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}

// RestoreFile 恢复文件（教师）
func RestoreFile(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "只有教师可以恢复文件"})
	}
	fileIDStr := c.Params("fileId")
	if fileIDStr == "" {
		return c.Status(400).JSON(fiber.Map{"error": "缺少文件ID"})
	}
	fileID, err := strconv.Atoi(fileIDStr)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效的文件ID"})
	}
	var exists bool
	err = database.Pool.QueryRow(c.Context(), "SELECT EXISTS(SELECT 1 FROM chat_files WHERE id=$1 AND is_deleted=true)", fileID).Scan(&exists)
	if err != nil || !exists {
		return c.Status(404).JSON(fiber.Map{"error": "文件不存在或未被删除"})
	}
	_, err = database.Pool.Exec(c.Context(),
		`UPDATE chat_files SET is_deleted = false, deleted_at = NULL WHERE id = $1`,
		fileID,
	)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}
