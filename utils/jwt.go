package utils

import (
	"crypto/rand"
	"log"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	jwtSecretMu sync.RWMutex
	JwtSecret   []byte
)

func SetJWTSecret(secret string) {
	jwtSecretMu.Lock()
	defer jwtSecretMu.Unlock()

	if secret == "" {
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			log.Fatalf("🛑 未配置 jwt.secret，且生成随机密钥失败: %v", err)
		}
		JwtSecret = random
		log.Println("⚠️  未配置 jwt.secret（config.properties）/ JWT_SECRET（环境变量），已生成随机密钥。" +
			"重启服务后所有已登录用户将需要重新登录。如果您处在生产环境中，请务必显式配置该项。")
		return
	}
	JwtSecret = []byte(secret)
}

// getSecret 供 GenerateToken / ParseToken 内部读取当前密钥。
func getSecret() []byte {
	jwtSecretMu.RLock()
	defer jwtSecretMu.RUnlock()
	return JwtSecret
}

type Claims struct {
	UserID   int    `json:"uid"`
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

func GenerateToken(userID int, username, role string) (string, error) {
	claims := Claims{
		UserID:   userID,
		Username: username,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   username,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(getSecret())
}

func ParseToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		return getSecret(), nil
	})
	if err != nil {
		return nil, err
	}
	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}
	return nil, jwt.ErrSignatureInvalid
}
