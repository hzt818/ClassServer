// Copyright (c) VisualClassroom. All rights reserved.

// 支付宝实名认证客户端（alipay.user.certify.open.*，RSA2 签名，仅用标准库实现）。
//
// 流程（教师注册身份核验）：
//  1. CertifyInitialize：提交姓名 + 身份证号 → 拿到 certify_id；
//  2. CertifyPageURL：构造带签名的网关跳转 URL，用户浏览器打开并完成刷脸/证件验证；
//  3. CertifyQuery：轮询认证结果，passed=true 即通过。
//
// 身份证号只在内存与本次 API 调用中出现，不落库；库中仅存 real_name 与是否通过。
package utils

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"visualclassroom/classserver/config"
)

// ErrAlipayNotConfigured 表示未配置支付宝开放平台密钥（此时应使用 mock 模式或提示管理员）。
var ErrAlipayNotConfigured = errors.New("支付宝实名认证未配置：请在 config.properties 填写 alipay.app_id / alipay.private_key / alipay.alipay_public_key")

// ErrAlipayCertifyFailed 表示支付宝侧返回业务失败（code != 10000）。
type AlipayBizError struct {
	Code    string
	SubMsg  string
}

func (e *AlipayBizError) Error() string {
	if e.SubMsg != "" {
		return "支付宝认证失败(" + e.Code + "): " + e.SubMsg
	}
	return "支付宝认证失败(" + e.Code + ")"
}

// AlipayConfig 从全局配置装载支付宝参数。
func LoadAlipayConfig() AlipayConfig {
	cfg := config.LoadConfig()
	return AlipayConfig{
		AppID:      cfg.AlipayAppID,
		PrivateKey: cfg.AlipayPrivateKey,
		PublicKey:  cfg.AlipayPublicKey,
		Gateway:    cfg.AlipayGateway,
		Mock:       cfg.AlipayMock,
	}
}

type AlipayConfig struct {
	AppID      string
	PrivateKey string
	PublicKey  string
	Gateway    string
	Mock       bool
}

// Enabled 判断真实渠道是否可用（三项密钥齐备）。
func (c AlipayConfig) Enabled() bool {
	return c.AppID != "" && c.PrivateKey != "" && c.PublicKey != ""
}

var alipayHTTPClient = &http.Client{Timeout: 15 * time.Second}

// ---------- 对外三步 ----------

// CertifyInitialize 第一步：提交实名信息，返回 certify_id。
// 身份证号仅通过本调用传给支付宝网关（HTTPS），不写日志、不落库。
func CertifyInitialize(cfg AlipayConfig, outerOrderNo, realName, certNo string) (string, error) {
	if !cfg.Enabled() {
		return "", ErrAlipayNotConfigured
	}
	bizContent, _ := json.Marshal(map[string]string{
		"outer_order_no": outerOrderNo,
		"cert_type":      "IDENTITY_CARD",
		"identity_type":  "CERT_INFO",
		"cert_name":      realName,
		"cert_no":        certNo,
	})
	resp, err := alipayRequest(cfg, "alipay.user.certify.open.initialize", string(bizContent))
	if err != nil {
		return "", err
	}
	certifyID, _ := resp["certify_id"].(string)
	if certifyID == "" {
		return "", fmt.Errorf("支付宝未返回 certify_id")
	}
	return certifyID, nil
}

// CertifyPageURL 第二步：构造用户浏览器跳转的认证页 URL（带 RSA2 签名）。
func CertifyPageURL(cfg AlipayConfig, certifyID string) (string, error) {
	if !cfg.Enabled() {
		return "", ErrAlipayNotConfigured
	}
	bizContent, _ := json.Marshal(map[string]string{"certify_id": certifyID})
	params := baseParams(cfg, "alipay.user.certify.open.open", string(bizContent))
	signed, err := signParams(cfg, params)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	for k, v := range signed {
		q.Set(k, v)
	}
	gateway := cfg.Gateway
	if !strings.HasPrefix(gateway, "https://") && !strings.HasPrefix(gateway, "http://") {
		return "", fmt.Errorf("支付宝网关地址必须以 http(s):// 开头")
	}
	return gateway + "?" + q.Encode(), nil
}

// CertifyQuery 第三步：查询认证结果。
func CertifyQuery(cfg AlipayConfig, certifyID string) (bool, error) {
	if !cfg.Enabled() {
		return false, ErrAlipayNotConfigured
	}
	bizContent, _ := json.Marshal(map[string]string{"certify_id": certifyID})
	resp, err := alipayRequest(cfg, "alipay.user.certify.open.query", string(bizContent))
	if err != nil {
		return false, err
	}
	passed, _ := resp["passed"].(bool)
	return passed, nil
}

// ---------- 协议底层 ----------

// baseParams 组装公共请求参数（不含 sign / sign_type 参与签名的部分）。
func baseParams(cfg AlipayConfig, method, bizContent string) map[string]string {
	gmt8 := time.FixedZone("GMT+8", 8*3600) // 支付宝要求北京时间格式
	return map[string]string{
		"app_id":      cfg.AppID,
		"method":      method,
		"format":      "JSON",
		"charset":     "utf-8",
		"version":     "1.0",
		"timestamp":   time.Now().In(gmt8).Format("2006-01-02 15:04:05"),
		"biz_content": bizContent,
	}
}

// signParams 计算签名并补上 sign_type / sign 两个字段。
// 签名串：按 key 升序拼接 "k=v&"（不含 sign、sign_type，值不做 URL 转义）。
func signParams(cfg AlipayConfig, params map[string]string) (map[string]string, error) {
	privateKey, err := parsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("解析应用私钥失败: %w", err)
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf strings.Builder
	for _, k := range keys {
		if k == "sign" || k == "sign_type" || params[k] == "" {
			continue
		}
		if buf.Len() > 0 {
			buf.WriteByte('&')
		}
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(params[k])
	}
	digest := sha256.Sum256([]byte(buf.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return nil, fmt.Errorf("RSA2 签名失败: %w", err)
	}
	out := make(map[string]string, len(params)+2)
	for k, v := range params {
		out[k] = v
	}
	out["sign_type"] = "RSA2"
	out["sign"] = base64.StdEncoding.EncodeToString(sig)
	return out, nil
}

// alipayRequest 发起网关调用：POST 表单 → 验签 → 解出业务响应节点。
func alipayRequest(cfg AlipayConfig, method, bizContent string) (map[string]any, error) {
	params := baseParams(cfg, method, bizContent)
	signed, err := signParams(cfg, params)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	for k, v := range signed {
		form.Set(k, v)
	}
	if !strings.HasPrefix(cfg.Gateway, "https://") && !strings.HasPrefix(cfg.Gateway, "http://") {
		return nil, fmt.Errorf("支付宝网关地址必须以 http(s):// 开头")
	}
	resp, err := alipayHTTPClient.PostForm(cfg.Gateway, form)
	if err != nil {
		return nil, fmt.Errorf("请求支付宝网关失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取支付宝响应失败: %w", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析支付宝响应失败: %w", err)
	}

	// 响应节点 key 形如 "alipay_user_certify_open_initialize_response"
	var nodeKey string
	for k := range parsed {
		if strings.HasSuffix(k, "_response") {
			nodeKey = k
			break
		}
	}
	rawNode, ok := parsed[nodeKey]
	if !ok {
		return nil, fmt.Errorf("支付宝响应缺少业务节点")
	}
	// 验签：对 "<节点key>=<原始响应JSON>" 用支付宝公钥校验
	if signRaw, ok := parsed["sign"]; ok {
		if err := verifyAlipaySign(cfg.PublicKey, nodeKey+"="+string(rawNode), string(signRaw)); err != nil {
			return nil, fmt.Errorf("支付宝响应验签失败: %w", err)
		}
	}

	var node map[string]any
	if err := json.Unmarshal(rawNode, &node); err != nil {
		return nil, fmt.Errorf("解析支付宝业务响应失败: %w", err)
	}
	if code, _ := node["code"].(string); code != "10000" {
		subMsg, _ := node["sub_msg"].(string)
		return nil, &AlipayBizError{Code: code, SubMsg: subMsg}
	}
	return node, nil
}

// verifyAlipaySign 用支付宝公钥校验响应签名（RSA-SHA256）。
func verifyAlipaySign(publicKeyPEM, content, signB64 string) error {
	pub, err := parsePublicKey(publicKeyPEM)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(signB64)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(content))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig)
}

// parsePrivateKey 兼容 PKCS1 / PKCS8 PEM 与裸 base64（控制台常见格式）。
func parsePrivateKey(raw string) (*rsa.PrivateKey, error) {
	trimmed := strings.TrimSpace(raw)
	if block, _ := pem.Decode([]byte(trimmed)); block != nil {
		trimmed = string(block.Bytes)
	} else {
		// 裸 base64：补换行使其成为规范形态后再解码
		if b, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
			trimmed = string(b)
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey([]byte(trimmed)); err == nil {
		return key, nil
	}
	key8, err := x509.ParsePKCS8PrivateKey([]byte(trimmed))
	if err != nil {
		return nil, fmt.Errorf("无法解析私钥（需要 PKCS1/PKCS8）: %w", err)
	}
	rsaKey, ok := key8.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("私钥不是 RSA 类型")
	}
	return rsaKey, nil
}

// parsePublicKey 兼容 PKIX PEM 与裸 base64。
func parsePublicKey(raw string) (*rsa.PublicKey, error) {
	trimmed := strings.TrimSpace(raw)
	if block, _ := pem.Decode([]byte(trimmed)); block != nil {
		trimmed = string(block.Bytes)
	} else {
		if b, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
			trimmed = string(b)
		}
	}
	pub, err := x509.ParsePKIXPublicKey([]byte(trimmed))
	if err != nil {
		return nil, fmt.Errorf("无法解析支付宝公钥（需要 PKIX）: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("支付宝公钥不是 RSA 类型")
	}
	return rsaPub, nil
}
