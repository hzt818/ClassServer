// Copyright (c) 黄智韬. All rights reserved.

// 作文批改的"富化"环节：主批改完成（status=done）后在后台异步执行——
//  1. 名师点评：文本模型基于原文与批改意见生成总评 + 升华建议；
//  2. 图片批注：视觉模型在学生手写原图上定位问题句/好句/可提升处（JSON），
//     服务端用 Go 标准库把彩色标注框 + 编号徽标绘制到原图上，得到"批注图"。
//
// 两者独立容错：任一失败只影响对应栏目，不影响批改结果本身。
package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // 注册 PNG 解码器（学生上传的可能是 PNG 照片）
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"visualclassroom/classserver/database"

	"github.com/sashabaranov/go-openai"
)

const annotatedImageDir = "./static/uploads/essay_annotated"

func init() {
	if err := os.MkdirAll(annotatedImageDir, 0755); err != nil {
		log.Printf("创建批注图目录失败: %v", err)
	}
}

// imageAnnotation 一处图片批注：坐标为相对整图 0~1000 的归一化值。
type imageAnnotation struct {
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
	Kind string  `json:"kind"` // error / good / improve
	Note string  `json:"note"`
}

var annotationKindColor = map[string]color.RGBA{
	"error":   {R: 0xD9, G: 0x2B, B: 0x2B, A: 0xFF}, // 红：语法/拼写/标点错误
	"good":    {R: 0x1F, G: 0x8A, B: 0x3B, A: 0xFF}, // 绿：好句好词
	"improve": {R: 0xE0, G: 0x7A, B: 0x2E, A: 0xFF}, // 橙：可提升
}

// annotatePrompt 组装视觉模型的批注指令；附上 OCR 文本帮助模型对位。
func annotatePrompt(essayType, content string) string {
	label := essayTypeLabel[essayType]
	if label == "" {
		label = "作文"
	}
	contentBrief := truncateRunes(content, 800)
	return "你是阅卷老师。请在学生手写" + label + "的照片上做批注，定位以下三类位置：\n" +
		"1. kind=\"error\"：语法、拼写、用词、标点有错误的地方；\n" +
		"2. kind=\"good\"：值得表扬的好词好句；\n" +
		"3. kind=\"improve\"：表达平淡、可以提升或润色的地方。\n\n" +
		"参考的识别文本（可能有识别误差，以图片为准）：\n" + contentBrief + "\n\n" +
		"输出要求：只输出一个 JSON 数组，不要任何其他文字或代码块标记。每个元素格式：\n" +
		"{\"x\":0-1000,\"y\":0-1000,\"w\":0-1000,\"h\":0-1000,\"kind\":\"error|good|improve\",\"note\":\"15字以内中文批注\"}\n" +
		"x/y 是标注框左上角坐标，w/h 是框的宽高，全部相对整张图归一化到 0~1000。\n" +
		"最多 12 处，按重要性排序；找不到合适位置就输出空数组 []。"
}

// annotateEssayImage 调视觉模型为原图生成批注列表。
// 模型偶发不守 JSON 格式约定：解析失败自动换更严格的指令重试一次。
func annotateEssayImage(imagePath, essayType, content string) ([]imageAnnotation, error) {
	if !qwenAPIKeyed {
		return nil, errors.New(qwenErrUnavailable())
	}
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(imagePath))
	mime := "image/jpeg"
	if ext == ".png" {
		mime = "image/png"
	}
	imageURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		instruction := annotatePrompt(essayType, content)
		if attempt > 0 {
			instruction += "\n\n【再次强调】你的上一条输出无法解析。只输出一个 JSON 数组，以 [ 开头、以 ] 结尾，" +
				"不要输出任何解释、标题或代码块标记。"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		resp, err := qwenCreateChat(ctx, openai.ChatCompletionRequest{
			Model: qwenVisionModel,
			Messages: []openai.ChatCompletionMessage{
				{
					Role: openai.ChatMessageRoleUser,
					MultiContent: []openai.ChatMessagePart{
						{Type: openai.ChatMessagePartTypeText, Text: instruction},
						{Type: openai.ChatMessagePartTypeImageURL, ImageURL: &openai.ChatMessageImageURL{URL: imageURL}},
					},
				},
			},
		})
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if len(resp.Choices) == 0 {
			lastErr = errors.New("批注模型未返回内容")
			continue
		}
		anns, perr := parseAnnotationJSON(resp.Choices[0].Message.Content)
		if perr == nil {
			return anns, nil
		}
		lastErr = perr
	}
	return nil, lastErr
}

// parseAnnotationJSON 从模型输出中提取 JSON 数组（容忍代码块围栏与前后杂文）。
func parseAnnotationJSON(raw string) ([]imageAnnotation, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "["); i >= 0 {
		if j := strings.LastIndex(s, "]"); j > i {
			s = s[i : j+1]
		}
	}
	var anns []imageAnnotation
	if err := json.Unmarshal([]byte(s), &anns); err != nil {
		return nil, fmt.Errorf("批注 JSON 解析失败: %w", err)
	}
	cleaned := make([]imageAnnotation, 0, len(anns))
	for _, a := range anns {
		if a.Kind != "error" && a.Kind != "good" && a.Kind != "improve" {
			continue
		}
		if a.X < 0 || a.X > 1000 || a.Y < 0 || a.Y > 1000 || a.W < 0 || a.H < 0 {
			continue
		}
		if a.W < 8 || a.H < 4 { // 过小的框没有意义
			continue
		}
		note := strings.TrimSpace(a.Note)
		if len([]rune(note)) > 30 {
			note = string([]rune(note)[:30])
		}
		a.X = clampF(a.X, 0, 1000)
		a.Y = clampF(a.Y, 0, 1000)
		a.W = clampF(a.W, 0, 1000-a.X)
		a.H = clampF(a.H, 0, 1000-a.Y)
		a.Note = note
		cleaned = append(cleaned, a)
		if len(cleaned) >= 12 {
			break
		}
	}
	return cleaned, nil
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------- 批注图绘制（Go 标准库，无第三方图像依赖） ----------

// digitBitmap 3x5 像素数字位图（0-9），用于标注框编号徽标。
var digitBitmap = [10][5]string{
	{"111", "101", "101", "101", "111"}, // 0
	{"010", "110", "010", "010", "111"}, // 1
	{"111", "001", "111", "100", "111"}, // 2
	{"111", "001", "111", "001", "111"}, // 3
	{"101", "101", "111", "001", "001"}, // 4
	{"111", "100", "111", "001", "111"}, // 5
	{"111", "100", "111", "101", "111"}, // 6
	{"111", "001", "001", "010", "010"}, // 7
	{"111", "101", "111", "101", "111"}, // 8
	{"111", "101", "111", "001", "111"}, // 9
}

// drawAnnotatedImage 在原图上绘制标注框 + 编号徽标，返回 JPEG 编码结果。
func drawAnnotatedImage(srcPath string, anns []imageAnnotation) ([]byte, error) {
	src, err := decodeImageFile(srcPath)
	if err != nil {
		return nil, err
	}
	bounds := src.Bounds()
	img := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(img, img.Bounds(), src, bounds.Min, draw.Src)

	imgW, imgH := float64(bounds.Dx()), float64(bounds.Dy())
	const thickness = 3
	for i, a := range anns {
		col, ok := annotationKindColor[a.Kind]
		if !ok {
			col = annotationKindColor["improve"]
		}
		x0 := int(a.X / 1000 * imgW)
		y0 := int(a.Y / 1000 * imgH)
		x1 := x0 + int(a.W/1000*imgW)
		y1 := y0 + int(a.H/1000*imgH)
		x0 = clampInt(x0, 0, bounds.Dx()-2)
		y0 = clampInt(y0, 0, bounds.Dy()-2)
		x1 = clampInt(x1, x0+2, bounds.Dx()-1)
		y1 = clampInt(y1, y0+2, bounds.Dy()-1)
		strokeRect(img, x0, y0, x1, y1, thickness, col)
		drawNumberBadge(img, x0, y0, i+1, col, bounds.Dx(), bounds.Dy())
	}
	return encodeJPEG(img)
}

func encodeJPEG(img *image.RGBA) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeImageFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// strokeRect 画一个 thickness 像素宽的矩形边框。
func strokeRect(img *image.RGBA, x0, y0, x1, y1, thickness int, col color.RGBA) {
	for t := 0; t < thickness; t++ {
		for x := x0 + t; x <= x1-t; x++ {
			img.Set(x, y0+t, col)
			img.Set(x, y1-t, col)
		}
		for y := y0 + t; y <= y1-t; y++ {
			img.Set(x0+t, y, col)
			img.Set(x1-t, y, col)
		}
	}
}

// drawNumberBadge 在标注框左上角画一个实心圆徽标 + 白色编号数字。
func drawNumberBadge(img *image.RGBA, boxX, boxY, number int, col color.RGBA, imgW, imgH int) {
	const scale = 2 // 数字位图像素放大倍数
	digits := strconv.Itoa(number)
	// 数字宽高：每位 3x5，位间距 1
	textW := len(digits)*3*scale + (len(digits)-1)*scale
	textH := 5 * scale
	radius := textW/2 + 6
	if radius < 12 {
		radius = 12
	}
	cx := clampInt(boxX+radius+2, radius, imgW-radius-1)
	cy := clampInt(boxY+radius+2, radius, imgH-radius-1)

	// 实心圆
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			if dx*dx+dy*dy <= radius*radius {
				px, py := cx+dx, cy+dy
				if px >= 0 && px < imgW && py >= 0 && py < imgH {
					img.Set(px, py, col)
				}
			}
		}
	}
	// 白色数字，居中
	startX := cx - textW/2
	startY := cy - textH/2
	for di, ch := range digits {
		d := int(ch - '0')
		if d < 0 || d > 9 {
			continue
		}
		ox := startX + di*(3*scale+scale)
		for row := 0; row < 5; row++ {
			for colI := 0; colI < 3; colI++ {
				if digitBitmap[d][row][colI] != '1' {
					continue
				}
				for py := 0; py < scale; py++ {
					for px := 0; px < scale; px++ {
						x, y := ox+colI*scale+px, startY+row*scale+py
						if x >= 0 && x < imgW && y >= 0 && y < imgH {
							img.Set(x, y, color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF})
						}
					}
				}
			}
		}
	}
}

// ---------- 后台富化协程 ----------

// enrichEssayJob 批改完成后的异步富化：名师点评 + 图片批注，两步互相独立。
// 任何一步失败仅记录日志并留空对应栏目，绝不影响已完成的批改结果。
func enrichEssayJob(jobID int) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("🛑 批改富化协程 panic (id=%d): %v", jobID, r)
		}
	}()

	var essayType, title, content, imagePath, feedback, scoreText string
	var maxScore int
	err := database.Pool.QueryRow(context.Background(),
		"SELECT type, COALESCE(title,''), content, COALESCE(image_path,''), COALESCE(feedback,''), COALESCE(score_text,''), max_score FROM essay_submissions WHERE id=$1", jobID).
		Scan(&essayType, &title, &content, &imagePath, &feedback, &scoreText, &maxScore)
	if err != nil {
		log.Printf("🛑 富化读取批改记录失败 (id=%d): %v", jobID, err)
		return
	}

	// 1) 名师点评（文本模型，含升华建议）
	comment := generateMasterComment(essayType, title, content, feedback, scoreText, maxScore)
	if comment != "" {
		if _, err := database.Pool.Exec(context.Background(),
			"UPDATE essay_submissions SET teacher_comment=$1, updated_at=CURRENT_TIMESTAMP WHERE id=$2",
			comment, jobID); err != nil {
			log.Printf("⚠️ 名师点评入库失败 (id=%d): %v", jobID, err)
		}
	}

	// 2) 图片批注（视觉模型 + 服务端绘制）+ 手写风格分数章
	if imagePath == "" {
		return
	}
	anns, annErr := annotateEssayImage(imagePath, essayType, content)
	if annErr != nil {
		log.Printf("⚠️ 图片批注生成失败 (id=%d): %v（仍会叠加分数章）", jobID, annErr)
		anns = nil
	}
	if len(anns) == 0 && !scoreStampValid(scoreText, maxScore) {
		// 既无批注、分数也无效：不生成批注图
		return
	}
	srcPath := filepath.Join(essayImageDir, filepath.Base(imagePath))
	jpegData, drawErr := annotateAndStamp(srcPath, anns, scoreText, maxScore)
	if drawErr != nil {
		log.Printf("⚠️ 批注图绘制失败 (id=%d): %v", jobID, drawErr)
		return
	}
	annotatedName := fmt.Sprintf("%d_%d_annotated.jpg", jobID, time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(annotatedImageDir, annotatedName), jpegData, 0o644); err != nil {
		log.Printf("⚠️ 批注图保存失败 (id=%d): %v", jobID, err)
		return
	}
	annJSON := ""
	if len(anns) > 0 {
		if data, merr := json.Marshal(anns); merr == nil {
			annJSON = string(data)
		}
	}
	if _, err := database.Pool.Exec(context.Background(),
		"UPDATE essay_submissions SET annotated_path=$1, annotations=$2, updated_at=CURRENT_TIMESTAMP WHERE id=$3",
		annotatedName, annJSON, jobID); err != nil {
		log.Printf("⚠️ 批注结果入库失败 (id=%d): %v", jobID, err)
	}
}

// scoreStampValid 判断分数章是否可绘制。
func scoreStampValid(scoreText string, maxScore int) bool {
	if scoreText == "" || scoreText == "--" || scoreText == "错误" || maxScore <= 0 {
		return false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(scoreText), 64)
	return err == nil && v >= 0 && v <= float64(maxScore)
}

// generateMasterComment 生成名师点评；失败返回空串。
func generateMasterComment(essayType, title, content, feedback, scoreText string, maxScore int) string {
	if !qwenAPIKeyed {
		return ""
	}
	label := essayTypeLabel[essayType]
	if label == "" {
		label = "作文"
	}
	name := strings.TrimSpace(title)
	if name == "" {
		name = "（未识别题目）"
	}
	scoreDisp := strings.TrimSpace(scoreText)
	if scoreDisp == "" || scoreDisp == "--" {
		scoreDisp = "未评分"
	}
	userInput := fmt.Sprintf(
		"作文类型：%s\n题目：%s\n得分：%s / %d 分\n\n学生作文原文（OCR 识别，可能有少量识别误差）：\n%s\n\n批改意见（供你提炼判断，不要复述）：\n%s",
		label, name, scoreDisp, maxScore, truncateRunes(content, 2000), truncateRunes(feedback, 1500),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	resp, err := qwenCreateChat(ctx, chatRequest(masterCommentSystemPrompt, userInput))
	if err != nil || len(resp.Choices) == 0 {
		if err != nil {
			log.Printf("⚠️ 名师点评生成失败: %v", err)
		}
		return ""
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content)
}

// truncateRunes 按 rune 截断长文本，超出部分以省略号结尾。
func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "……"
}
