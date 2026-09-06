package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DBPath     string // SQLite 数据库文件路径（嵌入式，无需外部数据库服务）
	DBMaxConns int
	QwenAPIKey string // 通义千问 DashScope API Key（qwen.api_key / QWEN_API_KEY）
	JWTSecret  string
	Timezone   string
	Port       string
	BuildDev   bool
	VoiceDev   bool
	ConsoleDev bool
	Domain     string // app.domain
	Email      string // app.email

	// 支付宝实名认证（教师注册身份核验，alipay.user.certify.open.*）
	AlipayAppID      string // alipay.app_id / ALIPAY_APP_ID
	AlipayPrivateKey string // alipay.private_key / ALIPAY_PRIVATE_KEY（应用私钥 PEM 或裸 base64）
	AlipayPublicKey  string // alipay.alipay_public_key / ALIPAY_PUBLIC_KEY（支付宝公钥，验签用）
	AlipayGateway    string // alipay.gateway_url，默认官方网关 https://openapi.alipay.com/gateway.do
	AlipayMock       bool   // alipay.mock / ALIPAY_MOCK：未配置真实密钥时的本地模拟认证（仅开发用）

	// 千问模型选型（OpenAI 兼容模式；留空用默认）
	QwenTextModel     string // qwen.text_model：文本主力，默认 qwen-max
	QwenVisionModel   string // qwen.vision_model：视觉主力，默认 qwen-vl-max-latest
	QwenFallbackModel string // qwen.fallback_model：文本降级，默认 qwen-plus
	QwenSearch        bool   // qwen.search：批改时允许模型联网搜索核实，默认 true

	GradingConcurrency int // qwen.grading_concurrency：同时批改的作文数上限，默认 6

	SeedDemoAccounts bool // app.seed_demo_accounts：是否预置调试账户（admin01-10/u001-100），默认 false 不创建

	// 聊天室 AI：Bing 本地搜索（RAG）
	BingEnabled bool   // search.bing_enabled：聊天 AI 提问时本地检索 Bing 并注入参考结果，默认 true
	BingAPIKey  string // search.bing_api_key：Bing Web Search API v7 密钥；留空走免密网页抓取
	BingCount   int    // search.bing_count：每次注入的结果条数，默认 5（1~10）

	// 文生图模型（聊天室"画：xxx"）
	QwenImageModel string // qwen.image_model：默认 qwen-image-3.0（不可用时可用 wanx2.1-t2i-turbo）
}

func LoadConfig() *Config {
	props := loadPropertiesFile(configFilePath())

	return &Config{
		DBPath:     getConfigValue(props, "DB_PATH", "db.path", "./data/classroom.db"),
		DBMaxConns: getConfigValueAsInt(props, "DB_MAX_CONNS", "db.max_conns", 10),
		QwenAPIKey: getConfigValue(props, "QWEN_API_KEY", "qwen.api_key", ""),
		JWTSecret:  getConfigValue(props, "JWT_SECRET", "jwt.secret", ""),
		Timezone:   getConfigValue(props, "APP_TIMEZONE", "app.timezone", "UTC"),
		Port:       getConfigValue(props, "PORT", "app.port", "8900"),
		BuildDev:   getConfigValueAsBool(props, "BUILD_DEV", "build.dev", false),
		VoiceDev:   getConfigValueAsBool(props, "VOICE_DEV", "voice.dev", false),
		ConsoleDev: getConfigValueAsBool(props, "CONSOLE_DEV", "console.dev", false),
		Domain:     getConfigValue(props, "APP_DOMAIN", "app.domain", ""),
		Email:      getConfigValue(props, "APP_EMAIL", "app.email", ""),

		AlipayAppID:      getConfigValue(props, "ALIPAY_APP_ID", "alipay.app_id", ""),
		AlipayPrivateKey: getConfigValue(props, "ALIPAY_PRIVATE_KEY", "alipay.private_key", ""),
		AlipayPublicKey:  getConfigValue(props, "ALIPAY_PUBLIC_KEY", "alipay.alipay_public_key", ""),
		AlipayGateway:    getConfigValue(props, "ALIPAY_GATEWAY", "alipay.gateway_url", "https://openapi.alipay.com/gateway.do"),
		AlipayMock:       getConfigValueAsBool(props, "ALIPAY_MOCK", "alipay.mock", false),

		QwenTextModel:     getConfigValue(props, "QWEN_TEXT_MODEL", "qwen.text_model", ""),
		QwenVisionModel:   getConfigValue(props, "QWEN_VISION_MODEL", "qwen.vision_model", ""),
		QwenFallbackModel: getConfigValue(props, "QWEN_FALLBACK_MODEL", "qwen.fallback_model", ""),
		QwenSearch:        getConfigValueAsBool(props, "QWEN_SEARCH", "qwen.search", true),

		GradingConcurrency: getConfigValueAsInt(props, "QWEN_GRADING_CONCURRENCY", "qwen.grading_concurrency", 6),

		SeedDemoAccounts: getConfigValueAsBool(props, "SEED_DEMO_ACCOUNTS", "app.seed_demo_accounts", false),

		BingEnabled:    getConfigValueAsBool(props, "SEARCH_BING_ENABLED", "search.bing_enabled", true),
		BingAPIKey:     getConfigValue(props, "BING_API_KEY", "search.bing_api_key", ""),
		BingCount:      getConfigValueAsInt(props, "SEARCH_BING_COUNT", "search.bing_count", 5),
		QwenImageModel: getConfigValue(props, "QWEN_IMAGE_MODEL", "qwen.image_model", "qwen-image-3.0"),
	}
}

func configFilePath() string {
	if p := os.Getenv("CONFIG_FILE"); p != "" {
		return p
	}
	return "config.properties"
}

// EnsureJWTSecret 在未配置 jwt.secret 时生成随机密钥并写回配置文件，
// 既避免"重启后全员掉线"，也避免所有部署共用同一内置密钥。
// 密钥只保存在教师本机，请勿外传 config.properties。
func EnsureJWTSecret(cfg *Config) {
	if cfg.JWTSecret != "" {
		return
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		log.Printf("⚠️  生成 JWT 密钥失败，将使用每次启动随机的临时密钥（重启后需重新登录）: %v", err)
		return
	}
	generated := hex.EncodeToString(random)
	if err := persistConfigValue("jwt.secret", generated); err != nil {
		log.Printf("⚠️  JWT 密钥写回配置文件失败（将使用临时密钥，重启后需重新登录）: %v", err)
		return
	}
	cfg.JWTSecret = generated
	log.Println("✅ 已生成随机 JWT 签名密钥并写回 config.properties（请勿外传该文件）")
}

// persistConfigValue 就地更新配置文件中的单个键；文件不存在时创建。
func persistConfigValue(key, value string) error {
	path := configFilePath()
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(data), "\n")
	}
	prefixes := []string{key + "=", key + " =", key + ":", key + " :"}
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range prefixes {
			if strings.HasPrefix(trimmed, prefix) {
				lines[i] = key + "=" + value
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return os.WriteFile(path, []byte(out), 0o600)
}

func loadPropertiesFile(path string) map[string]string {
	props := make(map[string]string)
	file, err := os.Open(path)
	if err != nil {
		log.Printf("Configuration file %s not found, using environment variables / default values (if you need database configuration, please create this file, you can refer to config.properties)", path)
		return props
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		idx := strings.IndexAny(line, "=:")
		if idx < 0 {
			log.Printf("INVALID format in config file %s at line %d, skipped: %s", path, lineNo, line)
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if key == "" {
			continue
		}
		props[key] = val
	}
	if scanner.Err() != nil {
		log.Printf("ERROR reading config file %s: %v", path, scanner.Err())
	}
	return props
}

func getConfigValue(props map[string]string, envKey, propKey, defaultVal string) string {
	if val := os.Getenv(envKey); val != "" {
		return val
	}
	if val, ok := props[propKey]; ok && val != "" {
		return val
	}
	return defaultVal
}

func getConfigValueAsInt(props map[string]string, envKey, propKey string, defaultVal int) int {
	raw := getConfigValue(props, envKey, propKey, "")
	if raw == "" {
		return defaultVal
	}
	i, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("The value %q for configuration item %s/%s is NOT a valid number, using the default value %d", envKey, propKey, raw, defaultVal)
		return defaultVal
	}
	return i
}

func getConfigValueAsBool(props map[string]string, envKey, propKey string, defaultVal bool) bool {
	raw := strings.ToLower(strings.TrimSpace(getConfigValue(props, envKey, propKey, "")))
	if raw == "" {
		return defaultVal
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("The value %q for configuration item %s/%s is NOT a valid boolean, using the default value %v", envKey, propKey, raw, defaultVal)
		return defaultVal
	}
}
