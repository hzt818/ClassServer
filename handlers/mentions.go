package handlers

import (
	"strconv"
	"time"

	"visualclassroom/classserver/database"

	"github.com/gofiber/fiber/v3"
)

type Mention struct {
	ID              int        `json:"id"`
	MessageID       int        `json:"message_id"`
	MentionedUserID int        `json:"mentioned_user_id"`
	MentionerUserID int        `json:"mentioner_user_id"`
	MentionerName   string     `json:"mentioner_name"`
	RoomID          int        `json:"room_id"`
	ContentPreview  string     `json:"content_preview"`
	ReadAt          *time.Time `json:"read_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

// GetUnreadMentions 获取当前用户未读的提及记录
func GetUnreadMentions(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	rows, err := database.Pool.Query(c.Context(),
		`SELECT m.id, m.message_id, m.mentioned_user_id, m.mentioner_user_id,
		        u.username as mentioner_name, m.room_id, msg.content, m.read_at, m.created_at
		FROM mentions m
		JOIN users u ON u.id = m.mentioner_user_id
		JOIN messages msg ON msg.id = m.message_id
		WHERE m.mentioned_user_id = $1 AND m.read_at IS NULL
		ORDER BY m.created_at DESC`,
		userID,
	)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()
	var mentions []Mention
	for rows.Next() {
		var m Mention
		var content string
		err := rows.Scan(&m.ID, &m.MessageID, &m.MentionedUserID, &m.MentionerUserID,
			&m.MentionerName, &m.RoomID, &content, &m.ReadAt, &m.CreatedAt)
		if err != nil {
			continue
		}
		// 截取预览
		if len(content) > 50 {
			m.ContentPreview = content[:50] + "..."
		} else {
			m.ContentPreview = content
		}
		mentions = append(mentions, m)
	}
	return c.JSON(mentions)
}

// MarkMentionRead 标记单条提及为已读
func MarkMentionRead(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	mentionIDStr := c.Params("id")
	mentionID, err := strconv.Atoi(mentionIDStr)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效提及ID"})
	}
	// 确保该提及属于当前用户
	var ownerID int
	err = database.Pool.QueryRow(c.Context(),
		"SELECT mentioned_user_id FROM mentions WHERE id=$1", mentionID,
	).Scan(&ownerID)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "提及不存在"})
	}
	if ownerID != userID {
		return c.Status(403).JSON(fiber.Map{"error": "无权操作"})
	}
	_, err = database.Pool.Exec(c.Context(),
		"UPDATE mentions SET read_at = CURRENT_TIMESTAMP WHERE id = $1", mentionID,
	)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}

// MarkAllMentionsRead 标记当前用户所有提及为已读（用于批量）
func MarkAllMentionsRead(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	_, err := database.Pool.Exec(c.Context(),
		"UPDATE mentions SET read_at = CURRENT_TIMESTAMP WHERE mentioned_user_id = $1 AND read_at IS NULL",
		userID,
	)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}
