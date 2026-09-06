package handlers

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"visualclassroom/classserver/config"
	"visualclassroom/classserver/database"

	"github.com/gofiber/fiber/v3"
	"github.com/sashabaranov/go-openai"
)

// ---------- 作文原图存储 ----------
const essayImageDir = "./static/uploads/essay_images"

func init() {
	if err := os.MkdirAll(essayImageDir, 0755); err != nil {
		log.Printf("创建作文原图目录失败: %v", err)
	}
	n := config.LoadConfig().GradingConcurrency
	if n <= 0 {
		n = 6
	}
	if n > 16 {
		n = 16
	}
	gradingSem = make(chan struct{}, n)
}

// saveEssayImage 把 OCR 上传的原图落盘，返回文件名（不含目录，供存库使用）
func saveEssayImage(userID int, data []byte, origName string) (string, error) {
	ext := filepath.Ext(origName)
	if ext == "" {
		ext = ".jpg"
	}
	filename := fmt.Sprintf("%d_%d%s", userID, time.Now().UnixNano(), ext)
	savePath := filepath.Join(essayImageDir, filename)
	if err := os.WriteFile(savePath, data, 0644); err != nil {
		return "", err
	}
	return filename, nil
}

// CleanOrphanEssayImages 清理不再被任何草稿或批改记录引用的原图文件
// （草稿被删除/替换时只删数据库行，不动物理文件，避免与"先删草稿、再提交批改"的
// 前端时序冲突；孤儿文件交给这里按时间兜底清理）。
func CleanOrphanEssayImages() {
	entries, err := os.ReadDir(essayImageDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("🛑 读取作文原图目录失败: %v", err)
		}
		return
	}
	rows, err := database.Pool.Query(context.Background(),
		"SELECT image_path FROM essay_drafts WHERE image_path IS NOT NULL AND image_path <> '' UNION SELECT image_path FROM essay_submissions WHERE image_path IS NOT NULL AND image_path <> ''")
	if err != nil {
		log.Printf("🛑 查询作文原图引用失败: %v", err)
		return
	}
	referenced := make(map[string]bool)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			referenced[p] = true
		}
	}
	rows.Close()

	const orphanGracePeriod = 24 * time.Hour
	removed := 0
	for _, e := range entries {
		if e.IsDir() || referenced[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < orphanGracePeriod {
			continue
		}
		if err := os.Remove(filepath.Join(essayImageDir, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("🧹 已清理 %d 个未被引用的作文原图文件", removed)
	}
}

// ---------- 配置与常量 ----------
func essayMaxScore(essayType string) int {
	switch essayType {
	case "chinese":
		return 60
	case "english":
		return 15
	case "continuation":
		return 25
	default:
		return 100
	}
}

const englishAppliedWritingCriteria = `Criterion 1: Task Completion
- Level 0: The essay fails to convey any information and is completely irrelevant or illegible.
- Level 1-3: The essay does not complete the task; it misses major content points or includes irrelevant information due to misunderstanding the prompt.
- Level 4-6: The essay inadequately completes the task; it misses or unclearly describes major content points and includes some irrelevant information.
- Level 7-9: The essay adequately completes the task; it covers all major content points despite minor omissions.
- Level 10-12: The essay completely fulfills the task with only minor omissions.
- Level 13-15: The essay thoroughly fulfills the task, covering all content points.

Criterion 2: Content Relevance and Clarity
- Level 0: The content is too little to evaluate, irrelevant, or illegible.
- Level 1-3: The essay misses major content points and includes irrelevant information.
- Level 4-6: The essay misses or unclearly describes major content points and includes some irrelevant information.
- Level 7-9: The essay covers all major content points with some minor omissions.
- Level 10-12: The essay covers all major content points with only minor omissions.
- Level 13-15: The essay comprehensively covers all content points.

Criterion 3: Grammar and Vocabulary
- Level 0: The content is illegible or too little to evaluate.
- Level 1-3: The essay uses monotonous and limited grammar structures and vocabulary.
- Level 4-6: The essay uses monotonous and limited grammar structures and vocabulary, with some errors affecting comprehension.
- Level 7-9: The essay uses adequate grammar structures and vocabulary with minor errors that do not affect comprehension.
- Level 10-12: The essay uses appropriate grammar structures and vocabulary with few errors, mostly due to attempts at more complex structures.
- Level 13-15: The essay uses a wide range of grammar structures and vocabulary with minor errors due to attempts at higher-level language.

Criterion 4: Errors and Comprehension
- Level 0: The content is illegible or too little to evaluate.
- Level 1-3: Frequent grammar and vocabulary errors significantly affect comprehension.
- Level 4-6: Some grammar and vocabulary errors affect comprehension.
- Level 7-9: Minor grammar and vocabulary errors do not affect comprehension.
- Level 10-12: Few errors, mainly due to attempts at more complex structures, slightly affect comprehension.
- Level 13-15: Minor errors due to attempts at higher-level language do not affect comprehension.

Criterion 5: Coherence and Cohesion
- Level 0: The content is illegible or too little to evaluate.
- Level 1-3: The essay lacks connecting elements between sentences, leading to a lack of coherence.
- Level 4-6: The essay rarely uses connecting elements, resulting in a lack of coherence.
- Level 7-9: The essay uses simple connecting elements to maintain basic coherence.
- Level 10-12: The essay effectively uses connecting elements to maintain a compact structure.
- Level 13-15: The essay effectively uses connecting elements to maintain a compact and coherent structure.

Criterion 6: Writing Purpose
- Level 0: The content is illegible or too little to evaluate.
- Level 1-3: The essay fails to convey the intended message to the reader.
- Level 4-6: The essay unclearly conveys the intended message.
- Level 7-9: The essay achieves the intended writing purpose despite minor issues.
- Level 10-12: The essay clearly achieves the intended writing purpose with few issues.
- Level 13-15: The essay completely achieves the intended writing purpose.

Criterion 7: Organization and Logic
- Level 0: The content is disorganized and illogical.
- Level 1-3: The essay shows poor organization and lacks logical flow.
- Level 4-6: The essay is somewhat organized but lacks clear logic in places.
- Level 7-9: The essay is organized but may have minor logical inconsistencies.
- Level 10-12: The essay is well-organized and mostly logical with minor issues.
- Level 13-15: The essay is excellently organized and logical throughout.

Criterion 8: Tone, diction and Formality Suitability for the Scenario and related pragmatic language habits
- Level 0: The tone, diction and formality are inappropriate for the scenario (e.g., too informal for a formal notice or too formal for an invitation between friends).
- Level 1-3: The essay uses an inappropriate tone or diction and lacks the expected formality for the given scenario.
- Level 4-6: The essay uses a somewhat appropriate tone and diction but still lacks the expected formality for the scenario.
- Level 7-9: The tone is mostly appropriate but with minor issues (e.g., informal for a formal letter or too formal for personal correspondence).
- Level 10-12: The tone is appropriate for the scenario with only minor lapses.
- Level 13-15: The tone, diction and formality perfectly suit the scenario (e.g., warm and informal for personal letters between close friends, polite and formal for a prestigious person or public notice).`

const continuationWritingCriteria = `Criterion 1 Consistency and coherence in relation to the given text
Level 0 The continuation is completely inconsistent or incoherent with the given text.
Level 1-5 The continuation shows major inconsistencies with the original text and lacks coherence.
Level 6-10 Some inconsistencies with the original text exist, and the coherence is weak.
Level 11-15 The continuation is mostly consistent and coherent with the original text, though some minor issues are present.
Level 16-20 The continuation is consistent and coherent with the original text with only minor lapses.
Level 21-25 The continuation is perfectly consistent and coherent with the original text.

Criterion 2 Plausibility and completeness of the plot in the continuation
Level 0 The plot is completely implausible or incomplete.
Level 1-5 The plot is largely implausible or incomplete, leaving significant gaps.
Level 6-10 The plot has some implausibilities or incompleteness, but the general storyline is clear.
Level 11-15 The plot is plausible and fairly complete, though it may have minor gaps.
Level 16-20 The plot is both plausible and mostly complete, with only a few minor issues.
Level 21-25 The plot is entirely plausible and thoroughly complete, with no significant issues.

Criterion 3 Diversity of rhetorical devices or strategies
Level 0 No rhetorical devices or strategies are used.
Level 1-5 Very few rhetorical devices are used, and the variety is lacking.
Level 6-10 Some rhetorical devices are used, but the variety is limited.
Level 11-15 A fair range of rhetorical devices or strategies are used effectively.
Level 16-20 A diverse range of rhetorical devices are used skillfully, enhancing the continuation.
Level 21-25 A wide variety of rhetorical devices and strategies are used masterfully throughout.

Criterion 4 Tone, diction, and other language habits consistent with different characters
Level 0 The tone, diction, or language habits are entirely inconsistent with the characters.
Level 1-5 The tone or diction does not fit the characters well, with several inconsistencies.
Level 6-10 The tone or diction is somewhat consistent with the characters, but some inconsistencies remain.
Level 11-15 The tone, diction, and language habits are mostly consistent with the characters, with only minor discrepancies.
Level 16-20 The tone, diction, and language habits are consistent with the characters, with very few lapses.
Level 21-25 The tone, diction, and language habits perfectly match the characters, maintaining consistency throughout.

Criterion 5 Theme clarity and positivity in the continuation
Level 0 The theme is unclear, and there is no positive progression.
Level 1-5 The theme is unclear or weak, and positivity is mostly absent.
Level 6-10 The theme is somewhat clear but lacks focus or positivity in places.
Level 11-15 The theme is clear and positive, though it may be unevenly presented.
Level 16-20 The theme is clear and maintains a positive tone throughout with minor issues.
Level 21-25 The theme is crystal clear and consistently positive throughout the continuation.

Criterion 6 Content Relevance and Clarity according to the characters, plot, and theme
Level 0 The content is irrelevant to the characters, plot, and theme.
Level 1-5 The content is largely irrelevant or unclear, with many inconsistencies.
Level 6-10 The content is somewhat relevant but lacks clarity or precision in places.
Level 11-15 The content is mostly relevant and clear, though there may be minor inconsistencies.
Level 16-20 The content is relevant and clear, with very few lapses.
Level 21-25 The content is entirely relevant and clear, fitting perfectly with the characters, plot, and theme.

Criterion 7 Grammar, sentence patterns, and vocabulary
Level 0 The grammar, sentence patterns, and vocabulary are poor and illegible.
Level 1-5 Numerous grammatical errors and limited vocabulary that hinder comprehension.
Level 6-10 Some grammar and vocabulary issues are present, but they do not greatly affect comprehension.
Level 11-15 Grammar, sentence patterns, and vocabulary are generally correct, with minor errors that don't affect comprehension.
Level 16-20 Grammar, sentence patterns, and vocabulary are used effectively with few errors.
Level 21-25 Grammar, sentence patterns, and vocabulary are expertly handled, with minimal to no errors.

Criterion 8 Errors and Comprehension
Level 0 Too many errors make comprehension impossible.
Level 1-5 Frequent errors that greatly hinder comprehension.
Level 6-10 Some errors are present, but comprehension is not significantly affected.
Level 11-15 Few errors that slightly affect comprehension.
Level 16-20 Very few errors that barely affect comprehension.
Level 21-25 No significant errors; comprehension is clear and easy.

Criterion 9 Coherence and Cohesion
Level 0 Lacks any coherence or cohesion.
Level 1-5 Major issues with coherence and cohesion, making the text difficult to follow.
Level 6-10 Some coherence and cohesion are present, but there are still gaps in logical flow.
Level 11-15 The text is mostly coherent and cohesive, though there may be minor gaps.
Level 16-20 The text is coherent and cohesive, with only a few minor issues.
Level 21-25 The text is perfectly coherent and cohesive, flowing logically throughout.

Criterion 10 Organization and Logic
Level 0 No apparent organization or logic in the continuation.
Level 1-5 The organization is poor, and the logic is difficult to follow.
Level 6-10 The text is somewhat organized, but the logic is unclear in places.
Level 11-15 The text is mostly organized and logical, with some minor inconsistencies.
Level 16-20 The text is well-organized and mostly logical with few issues.
Level 21-25 The text is excellently organized and logically structured throughout.`

type Submission struct {
	ID        int       `json:"id"`
	UserID    int       `json:"user_id"`
	Username  string    `json:"username"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
}

// ---------- 页面 ----------
func ShowEssay(c fiber.Ctx) error {
	username := c.Locals("username").(string)
	role := c.Locals("role").(string)
	var fullName string
	_ = database.Pool.QueryRow(c.Context(), "SELECT full_name FROM users WHERE username=$1", username).Scan(&fullName)
	if fullName == "" {
		fullName = username
	}
	avatar, initial := loadAvatar(c, username, fullName)
	return c.Render("essay", fiber.Map{
		"Username":  username,
		"FullName":  fullName,
		"Role":      role,
		"Active":    "essay",
		"LastLogin": time.Now().Format("2006-01-02 15:04"),
		"Avatar":    avatar,
		"Initial":   initial,
	})
}

// ---------- OCR ----------
var essayNameLineRe = regexp.MustCompile(`^\s*姓名\s*[：:]\s*(.*)$`)
var essayNameAnywhereRe = regexp.MustCompile(`姓名\s*[：:]\s*([^\s，。；、,.;\n]+)`)

// addEssayDraft 把 OCR 结果落库为草稿
func addEssayDraft(ctx context.Context, userID int, kind, content, studentName, imagePath string) (int, error) {
	if kind == "title" {
		// 题目只保留一份，新识别的覆盖旧的
		if _, err := database.Pool.Exec(ctx, "DELETE FROM essay_drafts WHERE user_id=$1 AND kind='title'", userID); err != nil {
			return 0, err
		}
	}
	var seq int
	_ = database.Pool.QueryRow(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM essay_drafts WHERE user_id=$1", userID).Scan(&seq)
	var id int
	err := database.Pool.QueryRow(ctx,
		"INSERT INTO essay_drafts (user_id, kind, content, student_name, seq, image_path) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id",
		userID, kind, content, studentName, seq, nullIfEmpty(imagePath),
	).Scan(&id)
	return id, err
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func HandleOCR(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	file, err := c.FormFile("image")
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "请上传图片"})
	}
	src, err := file.Open()
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer src.Close()
	data, err := io.ReadAll(src)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	base64Image := base64.StdEncoding.EncodeToString(data)
	imageURL := "data:image/jpeg;base64," + base64Image

	// 原图落盘（用于后续在批改详情中与识别结果、批改结果对照展示）
	savedImagePath, imgErr := saveEssayImage(userID, data, file.Filename)
	if imgErr != nil {
		log.Printf("保存作文原图失败: %v", imgErr)
		savedImagePath = ""
	}

	ocrType := c.FormValue("type") // "title" or "essay"
	var prompt string
	if ocrType == "title" {
		prompt = "请完整识别图片中的作文题目、材料和写作要求，规则如下：\n" +
			"1. 按图片原文逐字输出：题目、导语、材料正文、写作要求一个都不能少，不要概括、不要改写、不要遗漏任何一段；\n" +
			"2. 保持图片中的原文分段与标点；\n" +
			"3. 只输出识别到的内容，不要输出任何解释、点评或多余文字。" +
			"识别不清的字用「□」占位，不要猜测编造。"
	} else {
		prompt = "你是精准的 OCR 引擎。请逐字识别图片中的手写文字，规则如下：\n" +
			"1. 仅做光学字符识别，不翻译、不改写、不解释、不总结、不修正错别字。\n" +
			"2. 学生姓名通常写在稿纸/答题卡开头的姓名栏、右上角、左上角，有时在正文末尾；" +
			"图片中只要出现\"姓名\"字样（如\"姓名：张三\"\"姓名 张三\"），就把它识别出来：\n" +
			"   - 找到姓名：输出第一行严格为「姓名：XXX」（XXX 为姓名原文，不要加引号或多余字）；\n" +
			"   - 未找到任何姓名：第一行输出「姓名：无」。\n" +
			"3. 从第二行起输出完整正文，保持原文段落与标点，一字不差。\n" +
			"4. 如果图片中有涂改/划掉的文字，识别可见的最终版本；识别不清的字用「□」占位，不要猜测编造。"
	}

	if !qwenAPIKeyed {
		return c.Status(500).JSON(fiber.Map{"error": qwenErrUnavailable()})
	}

	ctx := c.Context()
	req := openai.ChatCompletionRequest{
		Model: qwenVisionModel,
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleUser,
				MultiContent: []openai.ChatMessagePart{
					{Type: openai.ChatMessagePartTypeText, Text: prompt},
					{Type: openai.ChatMessagePartTypeImageURL, ImageURL: &openai.ChatMessageImageURL{URL: imageURL}},
				},
			},
		},
	}
	resp, err := qwenCreateChat(ctx, req)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	if len(resp.Choices) == 0 {
		return c.Status(500).JSON(fiber.Map{"error": "未识别出内容"})
	}
	text := strings.TrimSpace(resp.Choices[0].Message.Content)

	// 识别结果立即落库
	if ocrType == "title" {
		id, err := addEssayDraft(context.Background(), userID, "title", text, "", savedImagePath)
		if err != nil {
			log.Printf("保存题目草稿失败: %v", err)
		}
		return c.JSON(fiber.Map{"text": text, "draft_id": id, "kind": "title", "image_path": savedImagePath})
	}

	studentName := ""
	contentText := text
	lines := strings.SplitN(text, "\n", 2)
	if len(lines) > 0 {
		if m := essayNameLineRe.FindStringSubmatch(lines[0]); m != nil {
			if m[1] != "" && m[1] != "无" {
				studentName = strings.TrimSpace(m[1])
			}
			if len(lines) > 1 {
				contentText = strings.TrimSpace(lines[1])
			} else {
				contentText = ""
			}
		}
	}
	// 兜底：第一行不是标准格式时，在全文任意位置找"姓名：XXX"
	if studentName == "" {
		for _, m := range essayNameAnywhereRe.FindAllStringSubmatch(text, -1) {
			name := strings.TrimSpace(m[1])
			if name != "" && name != "无" {
				studentName = name
				break
			}
		}
	}
	id, err := addEssayDraft(context.Background(), userID, "essay", contentText, studentName, savedImagePath)
	if err != nil {
		log.Printf("保存作文草稿失败: %v", err)
	}
	return c.JSON(fiber.Map{"text": contentText, "student_name": studentName, "draft_id": id, "kind": "essay", "image_path": savedImagePath})
}

// ---------- 草稿箱管理 ----------
type EssayDraft struct {
	ID          int    `json:"id"`
	Kind        string `json:"kind"`
	Content     string `json:"content"`
	StudentName string `json:"student_name"`
	ImagePath   string `json:"image_path"`
}

func GetEssayDrafts(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	rows, err := database.Pool.Query(c.Context(),
		"SELECT id, kind, content, COALESCE(student_name,''), COALESCE(image_path,'') FROM essay_drafts WHERE user_id=$1 ORDER BY seq ASC", userID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()
	drafts := make([]EssayDraft, 0)
	for rows.Next() {
		var d EssayDraft
		if err := rows.Scan(&d.ID, &d.Kind, &d.Content, &d.StudentName, &d.ImagePath); err == nil {
			drafts = append(drafts, d)
		}
	}
	return c.JSON(drafts)
}

func DeleteEssayDrafts(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	var req struct {
		IDs []int `json:"ids"`
	}
	if err := c.Bind().JSON(&req); err != nil || len(req.IDs) == 0 {
		return c.Status(400).JSON(fiber.Map{"error": "参数错误"})
	}
	_, err := database.Pool.Exec(c.Context(),
		"DELETE FROM essay_drafts WHERE id = ANY($1) AND user_id=$2", req.IDs, userID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}

// ---------- 服务器端流式基础 ----------
var (
	essayStreamsMu sync.Mutex
	essayStreams   = make(map[int]*aiStreamState)
)

func registerEssayStream(jobID int) *aiStreamState {
	s := &aiStreamState{}
	essayStreamsMu.Lock()
	essayStreams[jobID] = s
	essayStreamsMu.Unlock()
	return s
}

func getEssayStream(jobID int) *aiStreamState {
	essayStreamsMu.Lock()
	defer essayStreamsMu.Unlock()
	return essayStreams[jobID]
}

func removeEssayStream(jobID int) {
	essayStreamsMu.Lock()
	delete(essayStreams, jobID)
	essayStreamsMu.Unlock()
}

func flushEssayContent(jobID int, feedback string, streaming bool, status string, scoreText *string) {
	_, err := database.Pool.Exec(context.Background(),
		"UPDATE essay_submissions SET feedback=$1, is_streaming=$2, status=$3, score_text=COALESCE($4, score_text), updated_at=CURRENT_TIMESTAMP WHERE id=$5",
		feedback, streaming, status, scoreText, jobID)
	if err != nil {
		log.Printf("🛑 更新作文批改进度失败 (id=%d): %v", jobID, err)
	}
}

// ---------- 分数提取 ----------
var (
	essayStrictScoreRe   = regexp.MustCompile(`总分\s*[：:]\s*(\d{1,3}(?:\.\d+)?)\s*分?`)
	essayFallbackScoreRe = regexp.MustCompile(`(?:得分|评分|总分)[：:]\s*(\d+(?:\.\d+)?)`)
)

func extractScoreGo(text string, maxScore int) string {
	if text == "" {
		return ""
	}
	last := ""
	for _, m := range essayStrictScoreRe.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v >= 0 && v <= float64(maxScore) {
			last = m[1]
		}
	}
	if last != "" {
		return last
	}
	for _, m := range essayFallbackScoreRe.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v >= 0 && v <= float64(maxScore) {
			last = m[1]
		}
	}
	return last
}

// ---------- Prompt 构建 ----------

// clampTemperature 把批改温度收敛到 [0, 1]：0 最严格，1 最自由。
func clampTemperature(t float64) float64 {
	if t <= 0 {
		return 0
	}
	if t > 1 {
		return 1
	}
	return t
}

// temperatureCalibration 把批改温度映射为评分尺度的校准段：
// 温度越低批改越严格（从严扣分、高分难得），越高越自由（鼓励为主、给学生表达空间）。
func temperatureCalibration(t float64) string {
	switch {
	case t <= 0.2:
		return "【批改温度：非常严格】近乎按评卷标准执行：所有问题如实扣分、不放过任何失误，" +
			"高分必须留给出色作文；小瑕疵也要明确指出。"
	case t <= 0.4:
		return "【批改温度：偏严格】从严把握：问题如实指出并让分数反映不足，避免虚高；" +
			"高分只留给真正优秀的作文。"
	case t <= 0.6:
		return "【批改温度：标准】按实际水平客观给分：不苛责也不放水，达到哪一档标准就给哪一档分数，问题如实指出。"
	case t <= 0.8:
		return "【批改温度：偏自由】以鼓励为主：达到基本要求给中上分数，小问题点到为止，" +
			"多给建设性建议，善意理解学生想表达的意思。"
	default:
		return "【批改温度：非常自由】鼓励为主：善意理解学生的表达，侧重挖掘亮点与进步空间，" +
			"分数整体偏宽，明显优秀放心给高分；但硬性缺失（跑题、漏要点、结构残缺）仍须指出。"
	}
}

// buildEssayPrompt 组装批改提示词；maxScore 支持自定义满分（<=0 时用类型默认），
// temperature（0~1）控制批改尺度：越低越严格，越高越自由。
func buildEssayPrompt(essayType, content string, maxScore int, temperature float64) (prompt string) {
	if maxScore <= 0 {
		maxScore = essayMaxScore(essayType)
	}
	scale := temperatureCalibration(temperature)
	scoreFormatInstruction := fmt.Sprintf(
		"\n\n【格式要求】批改意见正文结束后，必须另起一行，且这一行只能严格按照如下格式输出总分，不要添加任何其他文字、符号或说明：\n总分：XX分\n（XX 为 0~%d 之间的整数，不要使用小数、范围或\"约\"等模糊表达，也不要加粗或使用其他符号包裹）",
		maxScore,
	)
	switch essayType {
	case "chinese":
		prompt = fmt.Sprintf(
			"你是一位语文老师，请对以下语文作文进行详细批改，指出优点、不足，给出修改建议，并给出评分（满分%d分）。\n\n"+
				"%s\n\n"+
				"拿不准的知识点（典故、常识、用法）可联网核实后再下结论。\n\n"+
				"批改意见之后，请依次输出两个章节（标题必须一字不差）：\n\n"+
				"【升格指导】\n"+
				"分别针对作文的\"开头\"\"中间\"\"结尾\"各给出 1-2 条升格建议：每条先一句话点明优化方向"+
				"（如\"强化场景代入感，突出精神传承的引子作用\"），再紧跟【例句】，写出一整句可直接替换"+
				"原文的升格表达（必须写出完整句子，不要只讲思路）。\n\n"+
				"【全文升格范文】\n"+
				"在保留学生原文立意、场景与事实的前提下，写一篇完整的升华版范文：符合题目文体与字数要求"+
				"（语文作文 500-800 字），吸收升格指导中的例句，立意更深远、结构更完整、语言更有感染力，"+
				"让学生可以整篇对照学习。范文正文全部使用中文。\n\n"+
				"学生作文：\n",
			maxScore, scale,
		) + content + scoreFormatInstruction
	case "english":
		prompt = fmt.Sprintf(
			"你是一位英语老师，请对以下英语应用文写作进行详细批改。\n\n"+
				"请参考以下官方分析型评分标准（共 8 个维度，每个维度都以 0-%d 分的等级描述呈现，"+
				"这些维度是从不同角度对同一篇作文整体水平的描述，请综合参考各维度的等级描述来判断作文的整体档次，"+
				"而不是把 8 个维度的分数相加）：\n\n%s\n\n"+
				"%s\n\n"+
				"对语法、搭配、格式规范拿不准时，可联网核实后再写进评语。\n\n"+
				"【要点与格式专项排查】请在给出评价前逐一检查：1）格式是否符合该文体要求（如称呼、落款、段落格式等）；"+
				"2）是否遗漏了原文题目要求的关键信息点；3）语气是否得体、是否符合写信人与收信人之间的身份关系；"+
				"4）是否存在被忽略的明显语法或时态错误。发现问题请在评语中具体指出，并让分数如实反映。\n\n"+
				"请用中文指出该作文在语法、词汇、格式、结构、内容切题、连贯性、语气得体性等方面的优点与不足，并给出具体的修改建议，然后给出评分（满分%d分）。\n\n"+
				"批改意见之后，请依次输出三个章节，标题必须一字不差：\n\n"+
				"【基础修改版】\n"+
				"尽量保留原文的句子结构和表达方式，只修正语法错误、拼写错误、词语搭配不当以及基本的逻辑问题，"+
				"不做词汇升级或内容改写，改动应尽可能少。同时必须严格保留原文的场景（包括写信人和收信人的身份关系）"+
				"与原文所有的主要事实和要点（例如请求建议、说明原因、提出邀请等），不能改变原文中任何事实信息。\n\n"+
				"【升格指导】\n"+
				"分别针对应用文的\"开头\"\"中间（正文要点）\"\"结尾\"各给出 1-2 条升格建议：用中文点明优化方向"+
				"（如\"补充当代青年扎根基层的具体事例，让号召更有说服力\"），再紧跟【例句】，写出可直接替换原文的"+
				"英文升格表达（必须写出完整英文句子，不要只讲思路）。\n\n"+
				"【全文升格范文】\n"+
				"在保留原文场景、写信人/收信人身份与全部事实要点的前提下，写一篇完整的升华版范文："+
				"吸收升格指导中的例句，使用更丰富高级的词汇与句型，打磨到高分水平，使学生可以整篇对照学习。"+
				"范文正文全部使用英文书写，不要输出中文译文。\n\n学生作文：\n",
			maxScore, englishAppliedWritingCriteria, scale, maxScore,
		) + content + scoreFormatInstruction
	case "continuation":
		prompt = fmt.Sprintf(
			"你是一位英语老师，请对以下读后续写进行详细批改。学生的续写内容通常分为两段，每段的开头句是题目给定的固定句子（段首句），"+
				"学生必须原样使用该句子作为本段的第一句话。\n\n"+
				"请参考以下官方分析型评分标准（共 10 个维度，每个维度都以 0-%d 分的等级描述呈现，"+
				"这些维度是从不同角度对同一篇续写整体水平的描述，请综合参考各维度的等级描述来判断续写的整体档次，"+
				"而不是把 10 个维度的分数相加）：\n\n%s\n\n"+
				"%s\n\n"+
				"【情节合理性专项排查——必须逐条执行，不能只是泛泛而谈】在给出评价和分数之前，请特别针对以下几类常见问题逐一检查续写内容，"+
				"哪怕语言表达通顺、情节表面上说得过去，只要出现下列情况，也要在批改意见中明确指出，并让 Criterion 2（情节合理性与完整性）"+
				"及总分受到相应影响，不能因为其他维度表现好就掩盖过去：\n"+
				"1. 人物的情绪、态度或人际关系是否与前文已经建立的基调自相矛盾（例如前文铺垫家人一直支持、理解，续写中却毫无过渡地变成完全不理睬、不说话）；\n"+
				"2. 场景切换是否存在逻辑漏洞（例如人物明明已经在某个场景中在场、正在观看/参与某事，续写却又莫名其妙切到另一个场景重复交代同一件事）；\n"+
				"3. 时间线、因果关系是否连贯（前后事件的先后顺序、因果推动是否说得通）；\n"+
				"4. 人物的行为反应是否符合原文已经塑造的性格（例如原文体现某人物勇敢、善于沟通，续写中却毫无铺垫地变得逃避、沉默）。\n"+
				"发现以上任意一类问题，都请在中文批改意见里具体点出是哪一句、哪个情节出了问题、为什么不合理，而不是只笼统地说\"情节基本合理\"。\n\n"+
				"请用中文指出该续写在情节合理性与完整性、与原文的衔接一致性、人物语气用词一致性、主题清晰度与正能量、"+
				"内容相关性、语法词汇、连贯衔接、组织逻辑等方面的优点与不足，并给出具体的修改建议，然后给出评分（满分%d分）。\n\n"+
				"批改意见之后，请依次输出三个章节，标题必须一字不差。在【基础修改版】与【全文升格范文】中，"+
				"你都必须首先从学生原文中识别出两段各自的开头句（段首句），并原封不动地保留这两个段首句，"+
				"分别作为对应段落的第一句话；段首句必须与原文完全相同，不得做任何改写、删减或移动。\n\n"+
				"【基础修改版】\n"+
				"段首句之后的内容，尽量保留学生原文的句子结构、用词和情节安排，只修正语法错误、拼写错误、"+
				"词语搭配不当以及基本的逻辑问题，不做词汇升级、修辞升级或情节改写，改动应尽可能少。\n\n"+
				"【升格指导】\n"+
				"分别针对续写的\"开头（衔接与前文呼应）\"\"中间（情节推进与细节）\"\"结尾（主题升华）\"各给出 1-2 条升格建议："+
				"用中文点明优化方向（如\"补上必要的心理过渡，去掉与前文重复交代的场景\"），再紧跟【例句】，"+
				"写出可直接替换原文的英文升格表达（必须写出完整英文句子，不要只讲思路）。"+
				"若情节合理性专项排查发现了逻辑漏洞，升格建议中要针对性给出修补方案。\n\n"+
				"【全文升格范文】\n"+
				"在高度遵循原给定文本的设定、情节走向和主题的前提下，写一篇完整的升华版续写（两段，原样保留段首句）："+
				"吸收升格指导中的例句，对词汇、句型和修辞手法进行升级润色，修补情节漏洞使情节自洽，"+
				"并对结尾的主题升华进行润色提升，使全文更连贯、更具可读性和感染力，让学生可以整篇对照学习。"+
				"范文正文全部使用英文书写，不要输出中文译文。\n\n学生续写内容：\n",
			maxScore, continuationWritingCriteria, scale, maxScore,
		) + content + scoreFormatInstruction
	default:
		prompt = fmt.Sprintf("请对以下作文进行综合批改，指出优缺点并给出评分（满分%d分）：\n", maxScore) + content + scoreFormatInstruction
	}
	return prompt
}

// ---------- 创建批改任务 ----------
func CreateEssayJob(c fiber.Ctx) error {
	var req struct {
		Content     string   `json:"content"`
		Type        string   `json:"type"`
		Title       string   `json:"title"`
		StudentName string   `json:"student_name"`
		ImagePath   string   `json:"image_path"`
		MaxScore    int      `json:"max_score"`   // 自定义满分（<=0 用类型默认）
		Temperature *float64 `json:"temperature"` // 批改温度 0~1：越低越严格，越高越自由
		Strictness  string   `json:"strictness"`  // 旧版三档尺度（兼容旧客户端）
	}
	if err := c.Bind().JSON(&req); err != nil || req.Content == "" {
		return c.Status(400).JSON(fiber.Map{"error": "作文内容不能为空"})
	}

	userID := c.Locals("userID").(int)
	username := c.Locals("username").(string)
	maxScore := req.MaxScore
	if maxScore <= 0 {
		maxScore = essayMaxScore(req.Type)
	}
	if maxScore > 200 {
		maxScore = 200 // 上限保护
	}
	// 温度：前端滑块传入 0~1；未传时按旧版三档折算，再兜底为标准 0.5
	temperature := 0.5
	if req.Temperature != nil {
		temperature = clampTemperature(*req.Temperature)
	} else {
		switch req.Strictness {
		case "lenient":
			temperature = 0.8
		case "strict":
			temperature = 0.2
		}
	}

	var jobID int
	err := database.Pool.QueryRow(c.Context(),
		"INSERT INTO essay_submissions (user_id, username, title, content, type, student_name, max_score, status, is_streaming, feedback, image_path) VALUES ($1, $2, $3, $4, $5, $6, $7, 'processing', true, '', $8) RETURNING id",
		userID, username, req.Title, req.Content, req.Type, req.StudentName, maxScore, nullIfEmpty(req.ImagePath),
	).Scan(&jobID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "创建批改任务失败: " + err.Error()})
	}

	registerEssayStream(jobID)
	go runEssayGrading(jobID, req.Type, req.Content, req.ImagePath, temperature, maxScore)

	return c.JSON(fiber.Map{"id": jobID, "status": "processing"})
}

// evaluateHandwriting 对学生作文原图做一次单独的书写清晰度/工整度点评（不涉及内容评分），
// 直接把图片交给视觉模型判断，返回一小段中文点评；没有原图或调用失败时返回空字符串，不影响主批改流程。
func evaluateHandwriting(imagePath string) string {
	if !qwenAPIKeyed || imagePath == "" {
		return ""
	}
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return ""
	}
	imageURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)
	ctx := context.Background()
	resp, err := qwenCreateChat(ctx, openai.ChatCompletionRequest{
		Model: qwenVisionModel,
		Messages: []openai.ChatCompletionMessage{
			{
				Role: openai.ChatMessageRoleUser,
				MultiContent: []openai.ChatMessagePart{
					{Type: openai.ChatMessagePartTypeText, Text: "请只针对这张学生手写作文照片的书写情况给出简短点评（字迹是否工整清晰、卷面是否整洁、涂改是否较多等），2-3句话即可，用中文回答。不要评价作文内容本身，只评价书写/卷面情况。"},
					{Type: openai.ChatMessagePartTypeImageURL, ImageURL: &openai.ChatMessageImageURL{URL: imageURL}},
				},
			},
		},
	})
	if err != nil || len(resp.Choices) == 0 {
		return ""
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content)
}

// ---------- 后台批改协程 ----------

// gradingSem 限制同时进行的批改流数量：批量导入时任务在服务端排队，
// 避免瞬时并发触发千问限流（429）。容量来自 qwen.grading_concurrency
// （默认 6，上限 16），配合前端并行提交加速批量批改。
var gradingSem chan struct{}

func acquireGradingSlot() { gradingSem <- struct{}{} }
func releaseGradingSlot() { <-gradingSem }

func runEssayGrading(jobID int, essayType, content, imagePath string, temperature float64, maxScore int) {
	state := getEssayStream(jobID)
	if state == nil {
		return
	}
	// panic 兜底：批改在独立协程运行，任何未预期异常都不能拖垮整个服务进程
	defer func() {
		if r := recover(); r != nil {
			log.Printf("🛑 批改协程 panic (id=%d): %v", jobID, r)
			errMsg := state.snapshot() + "\n\n[系统异常：批改中断，请对这篇重新批改]"
			flushEssayContent(jobID, errMsg, false, "error", nil)
		}
		state.finish()
		removeEssayStream(jobID)
	}()

	if !qwenAPIKeyed {
		errMsg := "错误：" + qwenErrUnavailable()
		state.append(errMsg)
		flushEssayContent(jobID, errMsg, false, "error", nil)
		return
	}

	// 排队等待空位（批量导入时最多 3 篇并行批改，其余排队）
	acquireGradingSlot()
	defer releaseGradingSlot()

	prompt := buildEssayPrompt(essayType, content, maxScore, temperature)
	// 可取消上下文：暂停按钮触发 state.cancel() 后，底层流立即断开
	ctx, cancelCtx := context.WithCancel(context.Background())
	state.mu.Lock()
	state.cancel = cancelCtx
	state.mu.Unlock()
	// 流式批改（带联网搜索核实）：搜索失败或模型不支持时自动降级为不带搜索重试。
	// 退避重试策略：限流(429)/网络抖动可重试；第 2 次仍失败则去掉搜索降级。
	lastFlush := time.Now()
	const flushInterval = 200 * time.Millisecond
	search := qwenSearchEnabled
	finishReason := ""
	streamErr := error(nil)
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt) * 2 * time.Second
			state.append(fmt.Sprintf("\n（AI 繁忙，%.0f 秒后重试第 %d 次…）\n", wait.Seconds(), attempt))
			flushEssayContent(jobID, state.snapshot(), true, "processing", nil)
			time.Sleep(wait)
		}
		finishReason, streamErr = streamChatWithSearch(ctx, essayGradingSystemPrompt, prompt, search, temperature, func(delta string) {
			state.append(delta)
			if time.Since(lastFlush) >= flushInterval {
				flushEssayContent(jobID, state.snapshot(), true, "processing", nil)
				lastFlush = time.Now()
			}
		})
		if streamErr == nil {
			break
		}
		// 带搜索失败且错误疑似与搜索参数相关 → 降级去掉搜索再试
		if search && strings.Contains(strings.ToLower(streamErr.Error()), "search") {
			log.Printf("批改搜索不可用，降级不带搜索 (id=%d): %v", jobID, streamErr)
			search = false
			continue
		}
		if !isRetryableQwenErr(streamErr) {
			break
		}
		log.Printf("批改流失败 (id=%d, 第 %d 次): %v", jobID, attempt+1, streamErr)
		// 第二次可重试失败后，降级去掉搜索（搜索会放大延迟与失败面）
		if attempt == 1 && search {
			search = false
		}
	}

	// 截断续写：输出撞上 max_tokens 上限（finish_reason=length）时，
	// 带着"已有内容"让模型从断点接着写，避免后半部分章节与总分行丢失
	// ——这是"有的报告有升格范文、有的没有"的根因。
	for cont := 0; streamErr == nil && finishReason == "length" && cont < 2; cont++ {
		state.append("\n（输出较长已触达单次上限，自动继续输出剩余内容…）\n")
		flushEssayContent(jobID, state.snapshot(), true, "processing", nil)
		contMessages := []map[string]string{
			{"role": "system", "content": essayGradingSystemPrompt},
			{"role": "user", "content": prompt},
			{"role": "assistant", "content": state.snapshot()},
			{"role": "user", "content": "你的上一条回复在输出中途被截断了。请从中断处直接继续输出剩余内容：不要重复任何已经输出过的文字，" +
				"保持章节标题一字不差，直到按【格式要求】在最后单独一行输出总分为止。"},
		}
		finishReason, streamErr = streamChatMessages(ctx, contMessages, false, temperature, func(delta string) {
			state.append(delta)
			if time.Since(lastFlush) >= flushInterval {
				flushEssayContent(jobID, state.snapshot(), true, "processing", nil)
				lastFlush = time.Now()
			}
		})
		log.Printf("批改截断续写 (id=%d, 第 %d 次, finish=%s): err=%v", jobID, cont+1, finishReason, streamErr)
	}
	if streamErr != nil {
		// 暂停导致的取消：保留已生成内容，落 paused 而非 error
		state.mu.Lock()
		pausedByUser := state.paused
		state.mu.Unlock()
		if pausedByUser {
			log.Printf("批改已暂停 (id=%d)", jobID)
			flushEssayContent(jobID, state.snapshot(), false, "paused", nil)
			return
		}
		errMsg := "请求 AI 失败: " + streamErr.Error()
		state.append(errMsg)
		flushEssayContent(jobID, errMsg, false, "error", nil)
		return
	}

	// 被暂停：保留已生成内容，标记 paused（不落分数，可后续重新批改）
	state.mu.Lock()
	paused := state.paused
	state.mu.Unlock()
	if paused {
		log.Printf("批改已暂停 (id=%d)", jobID)
		flushEssayContent(jobID, state.snapshot(), false, "paused", nil)
		return
	}

	final := state.snapshot()
	score := extractScoreGo(final, maxScore)
	var scorePtr *string
	if score != "" {
		scorePtr = &score
	} else {
		errStr := "--"
		scorePtr = &errStr
	}

	// 书写/卷面点评（如果有原图）：单独调用一次视觉模型，不影响上面已经从纯文本内容里提出的分数，
	// 追加在批改意见最后，作为补充信息而非评分依据的一部分。
	if imagePath != "" {
		if handwritingComment := evaluateHandwriting(imagePath); handwritingComment != "" {
			final = final + "\n\n【书写/卷面情况】\n" + handwritingComment
		}
	}

	flushEssayContent(jobID, final, false, "done", scorePtr)

	// 批改已完成：后台异步富化（名师点评 + 原图 AI 批注图），不阻塞"已完成"状态的呈现
	go enrichEssayJob(jobID)
}

// PauseEssayJob 暂停单个批改任务：取消底层流，保留已生成内容，状态置 paused。
func PauseEssayJob(c fiber.Ctx) error {
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	userID := c.Locals("userID").(int)
	role := c.Locals("role").(string)
	var owner int
	if err := database.Pool.QueryRow(c.Context(), "SELECT user_id FROM essay_submissions WHERE id=$1", id).Scan(&owner); err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "任务不存在"})
	}
	if owner != userID && role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权操作"})
	}
	state := getEssayStream(id)
	if state == nil {
		// 不在内存（已结束/本进程重启后）：直接置 paused
		_, _ = database.Pool.Exec(c.Context(),
			"UPDATE essay_submissions SET status='paused', is_streaming=false WHERE id=$1", id)
		return c.JSON(fiber.Map{"ok": true, "paused": true})
	}
	state.mu.Lock()
	already := state.paused
	state.paused = true
	cancel := state.cancel
	state.mu.Unlock()
	if cancel != nil {
		cancel() // 断开底层 AI 流，协程随即收尾并落 paused 状态
	}
	if already {
		return c.JSON(fiber.Map{"ok": true, "paused": true, "already": true})
	}
	return c.JSON(fiber.Map{"ok": true, "paused": true})
}

// ---------- 流式订阅 ----------
func StreamEssayJob(c fiber.Ctx) error {
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	userID := c.Locals("userID").(int)
	var owner int
	if err := database.Pool.QueryRow(c.Context(), "SELECT user_id FROM essay_submissions WHERE id=$1", id).Scan(&owner); err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "任务不存在"})
	}
	role := c.Locals("role").(string)
	if owner != userID && role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权访问"})
	}

	c.Set("Content-Type", "text/plain; charset=utf-8")
	c.Set("Transfer-Encoding", "chunked")
	c.Set("Cache-Control", "no-cache")
	c.Set("X-Accel-Buffering", "no")

	state := getEssayStream(id)
	if state == nil {
		// 已经跑完（或本进程重启后任务已在库里），直接把库里的最终内容吐回去
		var feedback string
		_ = database.Pool.QueryRow(c.Context(), "SELECT feedback FROM essay_submissions WHERE id=$1", id).Scan(&feedback)
		c.Response().SetBodyStreamWriter(func(w *bufio.Writer) {
			w.WriteString(feedback)
			w.Flush()
		})
		return nil
	}

	ch := state.subscribe()
	c.Response().SetBodyStreamWriter(func(w *bufio.Writer) {
		for chunk := range ch {
			if _, err := w.WriteString(chunk); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}
		}
	})
	return nil
}

// GetEssayJobImage 返回某次批改任务对应的学生作文原图
func GetEssayJobImage(c fiber.Ctx) error {
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	userID := c.Locals("userID").(int)
	role := c.Locals("role").(string)

	var owner int
	var imagePath string
	err = database.Pool.QueryRow(c.Context(),
		"SELECT user_id, COALESCE(image_path,'') FROM essay_submissions WHERE id=$1", id,
	).Scan(&owner, &imagePath)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "任务不存在"})
	}
	if owner != userID && role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权访问"})
	}
	if imagePath == "" {
		return c.Status(404).JSON(fiber.Map{"error": "该批改任务没有原图"})
	}

	fullPath := filepath.Join(essayImageDir, filepath.Base(imagePath))
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return c.Status(404).JSON(fiber.Map{"error": "原图文件丢失"})
	}
	return c.SendFile(fullPath)
}

// ---------- 任务列表 & 管理 ----------
type EssayJob struct {
	ID             int       `json:"id"`
	Title          string    `json:"title"`
	Content        string    `json:"content"`
	Type           string    `json:"type"`
	Username       string    `json:"username"`
	StudentName    string    `json:"student_name"`
	MaxScore       int       `json:"max_score"`
	Status         string    `json:"status"` // processing / done / error / paused
	IsStreaming    bool      `json:"is_streaming"`
	Feedback       string    `json:"feedback"`
	ScoreText      string    `json:"score_text"`
	TeacherEdited  bool      `json:"teacher_edited"`
	ImagePath      string    `json:"image_path"`
	TeacherComment string    `json:"teacher_comment"` // 名师点评（含升华建议）
	ShortComment   string    `json:"short_comment"`   // 教师逐篇短评（可空）
	AnnotatedPath  string    `json:"annotated_path"`  // AI 批注图文件名
	Annotations    string    `json:"annotations"`     // 批注列表 JSON
	Excellent      bool      `json:"excellent"`       // 优秀作文标记（教师流水线设置）
	CreatedAt      time.Time `json:"created_at"`
}

func GetEssayJobs(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	rows, err := database.Pool.Query(c.Context(),
		"SELECT id, title, content, type, COALESCE(student_name,''), max_score, status, is_streaming, feedback, COALESCE(score_text,''), teacher_edited, COALESCE(image_path,''), COALESCE(teacher_comment,''), COALESCE(short_comment,''), COALESCE(annotated_path,''), COALESCE(annotations,''), excellent, created_at FROM essay_submissions WHERE user_id=$1 AND is_deleted=false ORDER BY id DESC", userID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()
	jobs := make([]EssayJob, 0)
	for rows.Next() {
		var j EssayJob
		if err := rows.Scan(&j.ID, &j.Title, &j.Content, &j.Type, &j.StudentName, &j.MaxScore,
			&j.Status, &j.IsStreaming, &j.Feedback, &j.ScoreText, &j.TeacherEdited, &j.ImagePath,
			&j.TeacherComment, &j.ShortComment, &j.AnnotatedPath, &j.Annotations, &j.Excellent, &j.CreatedAt); err == nil {
			jobs = append(jobs, j)
		}
	}
	return c.JSON(jobs)
}

func DeleteEssayJobs(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	var req struct {
		IDs []int `json:"ids"`
	}
	if err := c.Bind().JSON(&req); err != nil || len(req.IDs) == 0 {
		return c.Status(400).JSON(fiber.Map{"error": "参数错误"})
	}
	_, err := database.Pool.Exec(c.Context(),
		"UPDATE essay_submissions SET is_deleted=true WHERE id = ANY($1) AND user_id=$2", req.IDs, userID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}

func UpdateEssayJobScore(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	role := c.Locals("role").(string)
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效ID"})
	}
	var req struct {
		Score string `json:"score"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "参数错误"})
	}
	var owner int
	if err := database.Pool.QueryRow(c.Context(), "SELECT user_id FROM essay_submissions WHERE id=$1", id).Scan(&owner); err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "任务不存在"})
	}
	if owner != userID && role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权操作"})
	}
	_, err = database.Pool.Exec(c.Context(),
		"UPDATE essay_submissions SET score_text=$1, teacher_edited=true, updated_at=CURRENT_TIMESTAMP WHERE id=$2", req.Score, id)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"success": true})
}

// ---------- 兼容旧接口（教师查看所有提交） ----------
func GetSubmissions(c fiber.Ctx) error {
	role := c.Locals("role").(string)
	if role != "teacher" {
		return c.Status(403).JSON(fiber.Map{"error": "无权限"})
	}
	rows, err := database.Pool.Query(c.Context(),
		"SELECT id, user_id, username, title, content, type, created_at FROM essay_submissions ORDER BY created_at DESC")
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	defer rows.Close()
	var subs []Submission
	for rows.Next() {
		var s Submission
		if err := rows.Scan(&s.ID, &s.UserID, &s.Username, &s.Title, &s.Content, &s.Type, &s.CreatedAt); err != nil {
			continue
		}
		subs = append(subs, s)
	}
	return c.JSON(subs)
}

// ---------- 导出 Word 批改报告 ----------

// GenerateModelEssay 按题目自动生成示范范文（教师备课/课堂展示用）。
func GenerateModelEssay(c fiber.Ctx) error {
	if !qwenAPIKeyed {
		return c.Status(503).JSON(fiber.Map{"error": qwenErrUnavailable()})
	}
	if !rateLimitAllow(c.IP()) {
		return c.Status(429).JSON(fiber.Map{"error": "操作过于频繁，请稍后再试"})
	}
	var req struct {
		Title string `json:"title"`
		Type  string `json:"type"` // chinese / english / continuation
	}
	if err := c.Bind().JSON(&req); err != nil || strings.TrimSpace(req.Title) == "" {
		return c.Status(400).JSON(fiber.Map{"error": "请先识别作文题目"})
	}
	if len([]rune(req.Title)) > 200 {
		req.Title = string([]rune(req.Title)[:200])
	}
	label := essayTypeLabel[req.Type]
	if label == "" {
		label = "语文作文"
	}
	userInput := "作文类型：" + label + "\n作文题目：\n" + strings.TrimSpace(req.Title)

	resp, err := qwenCreateChat(c.Context(), chatRequest(modelEssaySystemPrompt, userInput))
	if err != nil {
		return c.Status(502).JSON(fiber.Map{"error": "范文生成失败：" + err.Error()})
	}
	if len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		return c.Status(502).JSON(fiber.Map{"error": "范文生成失败，请重试"})
	}
	return c.JSON(fiber.Map{"essay": strings.TrimSpace(resp.Choices[0].Message.Content)})
}

var essayTypeLabel = map[string]string{
	"chinese":      "语文作文",
	"english":      "英语应用文",
	"continuation": "读后续写",
}
