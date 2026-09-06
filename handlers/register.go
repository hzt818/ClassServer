// Copyright (c) VisualClassroom. All rights reserved.

// 自助注册 + 教师支付宝实名认证。
// 学生注册后即可登录；教师注册需完成支付宝实名认证（alipay.user.certify.open.*）
// 才允许登录。身份证号仅驻留内存 30 分钟并只用于调用认证接口，不写日志、不落库。
package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"regexp"
	"sync"
	"time"

	"visualclassroom/classserver/database"
	"visualclassroom/classserver/utils"

	"github.com/gofiber/fiber/v3"
	"golang.org/x/crypto/bcrypt"
)

var (
	usernameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{2,19}$`)
	idCard18Re = regexp.MustCompile(`^\d{17}[\dXx]$`)
	idCard15Re = regexp.MustCompile(`^\d{15}$`)
)

// ---------- 认证会话（内存态，重启即失效，用户需重新提交注册即可再生成） ----------

type certifySession struct {
	UserID    int
	Username  string
	RealName  string
	CertNo    string // 仅内存，绝不落库/写日志
	CertifyID string // 支付宝返回的认证单号（mock 模式为空）
	Mock      bool
	ExpiresAt time.Time
}

var (
	certifyMu       sync.Mutex
	certifySessions = make(map[string]*certifySession) // regToken -> session
)

func newRegToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func cleanupCertifySessions() {
	now := time.Now()
	certifyMu.Lock()
	defer certifyMu.Unlock()
	for k, s := range certifySessions {
		if now.After(s.ExpiresAt) {
			delete(certifySessions, k)
		}
	}
}

// ---------- 简易 IP 频率限制（注册/认证端点，20 次/分钟） ----------

var (
	rlMu   sync.Mutex
	rlHits = make(map[string][]time.Time)
)

func rateLimitAllow(ip string) bool {
	const window = time.Minute
	const maxHits = 20
	now := time.Now()
	rlMu.Lock()
	defer rlMu.Unlock()
	hits := rlHits[ip][:0]
	for _, t := range rlHits[ip] {
		if now.Sub(t) < window {
			hits = append(hits, t)
		}
	}
	if len(hits) >= maxHits {
		rlHits[ip] = hits
		return false
	}
	rlHits[ip] = append(hits, now)
	return true
}

// ---------- 页面 ----------

// ShowRegister 显示注册页
func ShowRegister(c fiber.Ctx) error {
	lanIP, lanPort := utils.GetCurrentLANInfo()
	// 是否需要实名认证：配置了完整支付宝密钥 → 强制实名；
	// 未配置密钥但开启 mock → 保留模拟认证（开发用）；都未满足 → 跳过认证，教师直接注册成功。
	alipayCfg := utils.LoadAlipayConfig()
	alipayRequired := alipayCfg.Enabled() || alipayCfg.Mock
	return c.Render("register", fiber.Map{
		"LANIP":          lanIP,
		"LANPort":        lanPort,
		"AlipayRequired": alipayRequired,
	})
}

// ---------- 注册 ----------

// HandleRegister 创建账号。学生：立即可用；教师：视支付宝配置决定是否需要实名认证
// （已配置密钥 → 待认证 is_certified=0，完成认证方可登录；未配置 → 直接激活）。
func HandleRegister(c fiber.Ctx) error {
	if !rateLimitAllow(c.IP()) {
		return c.Status(429).JSON(fiber.Map{"error": "操作过于频繁，请稍后再试"})
	}

	alipayCfg := utils.LoadAlipayConfig()
	alipayRequired := alipayCfg.Enabled() || alipayCfg.Mock

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		FullName string `json:"full_name"`
		Role     string `json:"role"`
		RealName string `json:"real_name"`
		CertNo   string `json:"cert_no"`
	}
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "请求格式错误"})
	}

	req.Username = trimSpace(req.Username)
	req.Password = trimSpace(req.Password)
	req.FullName = trimSpace(req.FullName)
	req.Role = trimSpace(req.Role)
	req.RealName = trimSpace(req.RealName)
	req.CertNo = trimSpace(req.CertNo)

	if !usernameRe.MatchString(req.Username) {
		return c.Status(400).JSON(fiber.Map{"error": "用户名需以字母开头，3-20 位字母/数字/下划线"})
	}
	if len(req.Password) < 6 || len(req.Password) > 64 {
		return c.Status(400).JSON(fiber.Map{"error": "密码长度需为 6-64 位"})
	}
	if runeLen(req.FullName) < 1 || runeLen(req.FullName) > 30 {
		return c.Status(400).JSON(fiber.Map{"error": "请填写姓名（30 字以内）"})
	}
	if req.Role != "teacher" && req.Role != "student" {
		return c.Status(400).JSON(fiber.Map{"error": "角色不合法"})
	}
	// 仅在需要实名认证时校验真实姓名与身份证号（未配置认证则无需填写）
	if req.Role == "teacher" && alipayRequired {
		if runeLen(req.RealName) < 2 || runeLen(req.RealName) > 30 {
			return c.Status(400).JSON(fiber.Map{"error": "教师注册需填写真实姓名（与身份证一致）"})
		}
		if !idCard18Re.MatchString(req.CertNo) && !idCard15Re.MatchString(req.CertNo) {
			return c.Status(400).JSON(fiber.Map{"error": "身份证号格式不正确"})
		}
	}

	// 用户名唯一性预检（并发下仍以 UNIQUE 约束兜底）
	var exists int
	_ = database.Pool.QueryRow(c.Context(),
		"SELECT COUNT(1) FROM users WHERE username=$1", req.Username).Scan(&exists)
	if exists > 0 {
		return c.Status(409).JSON(fiber.Map{"error": "用户名已存在，请换一个"})
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("注册：生成密码哈希失败: %v", err)
		return c.Status(500).JSON(fiber.Map{"error": "系统错误，请稍后再试"})
	}

	certified := 1
	realName := ""
	needCertify := false
	// 教师且需要实名认证：账号置为待认证，完成认证后方可登录；
	// 未配置认证（含学生）直接激活。
	if req.Role == "teacher" && alipayRequired {
		certified = 0
		realName = req.RealName
		needCertify = true
	}
	if _, err := database.Pool.Exec(c.Context(),
		`INSERT INTO users (username, password_hash, role, full_name, is_certified, real_name)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		req.Username, string(hashed), req.Role, req.FullName, certified, realName); err != nil {
		// UNIQUE 冲突等一并按占用处理，避免暴露库细节
		log.Printf("注册：写入用户失败 (%s): %v", req.Username, err)
		return c.Status(409).JSON(fiber.Map{"error": "用户名已存在，请换一个"})
	}

	if !needCertify {
		log.Printf("注册：新账号 %s（角色 %s，无需认证）", req.Username, req.Role)
		return c.JSON(fiber.Map{"ok": true, "role": req.Role, "need_certify": false})
	}

	token, err := newRegToken()
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "系统错误，请稍后再试"})
	}
	var userID int
	_ = database.Pool.QueryRow(c.Context(),
		"SELECT id FROM users WHERE username=$1", req.Username).Scan(&userID)
	cleanupCertifySessions()
	certifyMu.Lock()
	certifySessions[token] = &certifySession{
		UserID:    userID,
		Username:  req.Username,
		RealName:  req.RealName,
		CertNo:    req.CertNo,
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	certifyMu.Unlock()
	log.Printf("注册：新教师账号 %s（待支付宝实名认证）", req.Username)
	return c.JSON(fiber.Map{"ok": true, "role": "teacher", "need_certify": true, "reg_token": token})
}

// StartCertify 第二步：发起支付宝实名认证，返回用户浏览器要打开的认证页 URL。
// 未配置密钥但 alipay.mock=true 时进入本地模拟（仅开发环境用）。
func StartCertify(c fiber.Ctx) error {
	if !rateLimitAllow(c.IP()) {
		return c.Status(429).JSON(fiber.Map{"error": "操作过于频繁，请稍后再试"})
	}
	token := c.Query("token")
	certifyMu.Lock()
	sess := certifySessions[token]
	certifyMu.Unlock()
	if sess == nil || time.Now().After(sess.ExpiresAt) {
		return c.Status(404).JSON(fiber.Map{"error": "认证会话不存在或已过期，请重新提交注册"})
	}

	cfg := utils.LoadAlipayConfig()
	if !cfg.Enabled() {
		if !cfg.Mock {
			return c.Status(400).JSON(fiber.Map{
				"error": "本服务尚未配置支付宝实名认证（需管理员在 config.properties 填写 alipay.app_id / alipay.private_key / alipay.alipay_public_key，或临时开启 alipay.mock 模拟）",
			})
		}
		log.Printf("实名认证：mock 模式（教师 %s）", sess.Username)
		certifyMu.Lock()
		sess.Mock = true
		certifyMu.Unlock()
		return c.JSON(fiber.Map{"ok": true, "mock": true, "certify_url": ""})
	}

	outerOrderNo := "MC" + time.Now().Format("20060102150405") + sess.Username
	certifyID, err := utils.CertifyInitialize(cfg, outerOrderNo, sess.RealName, sess.CertNo)
	if err != nil {
		log.Printf("实名认证：initialize 失败 (%s): %v", sess.Username, err)
		return c.Status(502).JSON(fiber.Map{"error": "发起支付宝认证失败：" + err.Error()})
	}
	certifyMu.Lock()
	sess.CertifyID = certifyID
	certifyMu.Unlock()
	certifyURL, err := utils.CertifyPageURL(cfg, certifyID)
	if err != nil {
		log.Printf("实名认证：构造认证页 URL 失败 (%s): %v", sess.Username, err)
		return c.Status(502).JSON(fiber.Map{"error": "构造支付宝认证页失败：" + err.Error()})
	}
	return c.JSON(fiber.Map{"ok": true, "mock": false, "certify_url": certifyURL})
}

// QueryCertify 轮询认证结果；通过则激活账号（is_certified=1）。
func QueryCertify(c fiber.Ctx) error {
	if !rateLimitAllow(c.IP()) {
		return c.Status(429).JSON(fiber.Map{"error": "操作过于频繁，请稍后再试"})
	}
	token := c.Query("token")
	certifyMu.Lock()
	sess := certifySessions[token]
	certifyMu.Unlock()
	if sess == nil || time.Now().After(sess.ExpiresAt) {
		return c.Status(404).JSON(fiber.Map{"error": "认证会话不存在或已过期，请重新提交注册"})
	}

	passed := false
	var errMsg string
	if sess.Mock {
		passed = true
	} else if sess.CertifyID != "" {
		cfg := utils.LoadAlipayConfig()
		ok, err := utils.CertifyQuery(cfg, sess.CertifyID)
		if err != nil {
			errMsg = err.Error()
		} else {
			passed = ok
		}
	}

	if !passed {
		status := fiber.StatusOK
		if errMsg != "" {
			status = fiber.StatusBadGateway
		}
		return c.Status(status).JSON(fiber.Map{"passed": false, "error": errMsg})
	}

	// 认证通过：激活账号并记录实名（身份证号不落库）
	if _, err := database.Pool.Exec(c.Context(),
		"UPDATE users SET is_certified = 1, real_name = $1 WHERE id = $2",
		sess.RealName, sess.UserID); err != nil {
		log.Printf("实名认证：激活账号失败 (%s): %v", sess.Username, err)
		return c.Status(500).JSON(fiber.Map{"passed": false, "error": "激活账号失败，请联系管理员"})
	}
	certifyMu.Lock()
	delete(certifySessions, token)
	certifyMu.Unlock()
	log.Printf("实名认证：教师 %s（%s）认证通过，账号已激活", sess.Username, sess.RealName)
	return c.JSON(fiber.Map{"passed": true, "username": sess.Username})
}

// ---------- 小工具 ----------

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
