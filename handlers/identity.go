// Copyright (c) VisualClassroom. All rights reserved.

package handlers

import (
	"visualclassroom/classserver/database"

	"github.com/gofiber/fiber/v3"
)

// loadAvatar 读取当前用户头像（可为空）。initial 取姓名首字（按 rune 截取，中文安全），
// 作为无头像时的圆形占位字。供侧栏身份块（头像 + 姓名）使用。
func loadAvatar(c fiber.Ctx, username, fullName string) (avatar, initial string) {
	_ = database.Pool.QueryRow(c.Context(),
		"SELECT COALESCE(avatar, '') FROM users WHERE username=$1", username).Scan(&avatar)
	name := fullName
	if name == "" {
		name = username
	}
	for _, r := range name {
		initial = string(r)
		break
	}
	return avatar, initial
}
