package models

type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Password string `json:"-"` // 不序列化密码
	Role     string `json:"role"`
	FullName string `json:"full_name"`
}
