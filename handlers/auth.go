package handlers

import (
	"log"
	"time"

	"visualclassroom/classserver/database"
	"visualclassroom/classserver/models"
	"visualclassroom/classserver/utils"

	"github.com/gofiber/fiber/v3"
	"golang.org/x/crypto/bcrypt"
)

// ShowLogin 显示登录页面
func ShowLogin(c fiber.Ctx) error {
	lanIP, lanPort := utils.GetCurrentLANInfo()
	return c.Render("login", fiber.Map{
		"Error":   "",
		"LANIP":   lanIP,
		"LANPort": lanPort,
	})
}

// HandleLogin 处理登录
func HandleLogin(c fiber.Ctx) error {
	username := c.FormValue("username")
	password := c.FormValue("password")
	lanIP, lanPort := utils.GetCurrentLANInfo()

	var user models.User
	var passwordHash string
	var certified bool
	query := `SELECT id, username, password_hash, role, full_name, COALESCE(is_certified, 1) FROM users WHERE username=$1`
	err := database.Pool.QueryRow(c.Context(), query, username).Scan(
		&user.ID, &user.Username, &passwordHash, &user.Role, &user.FullName, &certified,
	)

	// 抓取错误报错
	if err != nil {
		log.Printf("登录查询出错 (用户名: %s): %v", username, err)
		return c.Render("login", fiber.Map{
			"Error": "用户名或密码错误，请联系管理员", "LANIP": lanIP, "LANPort": lanPort,
		})
	}

	// 校验密码
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		log.Printf("密码校验失败 (用户名: %s): %v", username, err)
		return c.Render("login", fiber.Map{
			"Error": "用户名或密码错误，请联系管理员", "LANIP": lanIP, "LANPort": lanPort,
		})
	}

	// 教师账号必须先完成支付宝实名认证（历史内置账号 is_certified 默认为 1，不受影响）
	if user.Role == "teacher" && !certified {
		return c.Render("login", fiber.Map{
			"Error": "教师账号尚未完成支付宝实名认证，请先在注册页完成认证后再登录",
			"LANIP": lanIP, "LANPort": lanPort,
		})
	}

	// 更新最后登录时间（SQLite 用 CURRENT_TIMESTAMP 替代 PG 的 NOW()）
	_, _ = database.Pool.Exec(c.Context(), "UPDATE users SET last_login = CURRENT_TIMESTAMP WHERE id = $1", user.ID)

	// 生成 JWT
	token, err := utils.GenerateToken(user.ID, user.Username, user.Role)
	if err != nil {
		log.Printf("生成 Token 失败 (用户名: %s): %v", username, err)
		return c.Render("login", fiber.Map{
			"Error": "系统错误，请联系管理员", "LANIP": lanIP, "LANPort": lanPort,
		})
	}

	// 设置 Cookie
	cookie := fiber.Cookie{
		Name:     "auth_token",
		Value:    token,
		Expires:  time.Now().Add(24 * time.Hour),
		HTTPOnly: true,
		Secure:   false,
		SameSite: "Lax",
		Path:     "/",
	}
	c.Cookie(&cookie)

	if user.Role == "teacher" {
		return c.Redirect().To("/teacher/dashboard")
	}
	return c.Redirect().To("/student/dashboard")
}

// Logout 登出
func Logout(c fiber.Ctx) error {
	c.ClearCookie("auth_token")
	return c.Redirect().To("/login")
}
