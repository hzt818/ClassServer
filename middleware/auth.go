package middleware

import (
	"visualclassroom/classserver/utils"

	"github.com/gofiber/fiber/v3"
)

// AuthRequired 验证 JWT 并注入用户信息
func AuthRequired(c fiber.Ctx) error {
	token := c.Cookies("auth_token")
	if token == "" {
		return c.Redirect().To("/login")
	}
	claims, err := utils.ParseToken(token)
	if err != nil {
		c.ClearCookie("auth_token")
		return c.Redirect().To("/login")
	}
	c.Locals("userID", claims.UserID)
	c.Locals("username", claims.Username)
	c.Locals("role", claims.Role)
	return c.Next()
}

// RoleRequired 检查角色
func RoleRequired(role string) fiber.Handler {
	return func(c fiber.Ctx) error {
		userRole := c.Locals("role")
		if userRole != role {
			return c.Status(fiber.StatusForbidden).SendString("无权限访问")
		}
		return c.Next()
	}
}
