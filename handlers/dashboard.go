package handlers

import (
	"time"

	"visualclassroom/classserver/database"

	"github.com/gofiber/fiber/v3"
)

func renderDashboard(c fiber.Ctx, role string) error {
	username := c.Locals("username").(string)
	userID := c.Locals("userID").(int)
	var fullName string
	var lastLogin time.Time
	// CAST 使 SQLite 结果列保留 TIMESTAMP 声明类型，驱动才能把它解析成 time.Time
	query := `SELECT full_name, CAST(COALESCE(last_login, created_at) AS TIMESTAMP) FROM users WHERE username=$1`
	err := database.Pool.QueryRow(c.Context(), query, username).Scan(&fullName, &lastLogin)
	if err != nil {
		fullName = username
		lastLogin = time.Now()
	}
	avatar, initial := loadAvatar(c, username, fullName)
	return c.Render("dashboard", fiber.Map{
		"Username":  username,
		"FullName":  fullName,
		"Role":      role,
		"LastLogin": lastLogin.In(time.Local).Format("2006-01-02 15:04"),
		"Active":    "home",
		"UserID":    userID,
		"Avatar":    avatar,
		"Initial":   initial,
	})
}

func TeacherDashboard(c fiber.Ctx) error {
	return renderDashboard(c, "teacher")
}

func StudentDashboard(c fiber.Ctx) error {
	return renderDashboard(c, "student")
}

// About 页面
func About(c fiber.Ctx) error {
	username := c.Locals("username").(string)
	role := c.Locals("role").(string)
	var fullName string
	_ = database.Pool.QueryRow(c.Context(), "SELECT full_name FROM users WHERE username=$1", username).Scan(&fullName)
	if fullName == "" {
		fullName = username
	}
	avatar, initial := loadAvatar(c, username, fullName)
	return c.Render("about", fiber.Map{
		"Username":  username,
		"FullName":  fullName,
		"Role":      role,
		"Active":    "about",
		"LastLogin": time.Now().Format("2006-01-02 15:04"),
		"Avatar":    avatar,
		"Initial":   initial,
	})
}
