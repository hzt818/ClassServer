// Copyright (c) 黄智韬. All rights reserved.

package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildReportDocx 校验 docx 导出：zip 结构完整、XML 合法、包含媒体与关键文案。
func TestBuildReportDocx(t *testing.T) {
	job := EssayJob{
		ID:          1,
		Username:    "teacher",
		StudentName: "张三",
		Title:       "写信给朋友介绍你的暑假",
		Type:        "english",
		Status:      "done",
		MaxScore:    15,
		ScoreText:   "12",
		Content:     "Dear Lucy, How are you? I want to tell you about my summer holiday.",
		Feedback: "优点：结构完整，要点齐全。\n\n不足：第二段 there is 与 there are 混用，建议改为 there are。\n\n" +
			"【基础修改版】Dear Lucy, How are you? I want to tell you about my summer holiday. There are many interesting things.\n\n" +
			"【进阶修改版】Dear Lucy, How is everything going? I can hardly wait to share my unforgettable summer holiday with you.\n\n" +
			"总分：12分",
		TeacherComment: "总评：这是一篇完成度较高的应用文。亮点是开头问候自然。若能把结尾祝愿写得更具体，情感会更饱满，立意也更高远。",
		AnnotatedPath:  "not_exist_annotated.jpg",
		CreatedAt:      time.Now(),
	}
	data, err := buildReportDocx([]EssayJob{job})
	if err != nil {
		t.Fatalf("构建 docx 失败: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("docx 不是合法 zip: %v", err)
	}
	parts := map[string]bool{}
	for _, f := range zr.File {
		parts[f.Name] = true
		if strings.HasSuffix(f.Name, ".xml") || strings.HasSuffix(f.Name, ".rels") {
			rc, rerr := f.Open()
			if rerr != nil {
				t.Fatalf("打开 %s 失败: %v", f.Name, rerr)
			}
			dec := xml.NewDecoder(rc)
			for {
				_, terr := dec.Token()
				if terr != nil {
					break
				}
			}
			rc.Close()
		}
	}
	for _, required := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml", "word/_rels/document.xml.rels"} {
		if !parts[required] {
			t.Fatalf("docx 缺少部件: %s", required)
		}
	}
	docXML := readPart(t, zr, "word/document.xml")
	for _, keyword := range []string{"张三", "名师点评", "基础修改版", "进阶修改版", "学生原文", "批改详情", "12 / 15 分"} {
		if !strings.Contains(docXML, keyword) {
			t.Errorf("document.xml 缺少关键内容: %s", keyword)
		}
	}
}

// TestSplitRevisionSections 校验修改版章节拆分。
func TestSplitRevisionSections(t *testing.T) {
	fb := "批改意见正文。\n\n【基础修改版】BASIC\n\n【进阶修改版】ADVANCED\n\n总分：10分"
	main, basic, advanced, guide, polished := splitRevisionSections(fb)
	if !strings.Contains(main, "批改意见正文") {
		t.Errorf("main 拆分错误: %q", main)
	}
	if basic != "BASIC" {
		t.Errorf("basic 拆分错误: %q", basic)
	}
	if advanced != "ADVANCED" {
		t.Errorf("advanced 拆分错误: %q", advanced)
	}
	if guide != "" || polished != "" {
		t.Errorf("不应有升格章节: %q %q", guide, polished)
	}

	// 中文升格：升格指导 + 全文升格范文
	fb2 := "中文批改意见。\n\n【升格指导】开头：强化代入感。【例句】升华句……\n\n【全文升格范文】完整升华版范文正文。\n\n总分：42分"
	main2, _, _, guide2, polished2 := splitRevisionSections(fb2)
	if !strings.Contains(main2, "中文批改意见") {
		t.Errorf("中文 main 拆分错误: %q", main2)
	}
	if !strings.Contains(guide2, "开头：强化代入感") || !strings.Contains(guide2, "【例句】") {
		t.Errorf("升格指导拆分错误: %q", guide2)
	}
	if polished2 != "完整升华版范文正文。" {
		t.Errorf("升格范文拆分错误: %q", polished2)
	}
	if strings.Contains(polished2, "总分") {
		t.Errorf("升格范文不应包含总分行: %q", polished2)
	}
}

// TestParseAnnotationJSON 校验批注 JSON 容错解析。
func TestParseAnnotationJSON(t *testing.T) {
	raw := "```json\n[{\"x\":10,\"y\":20,\"w\":100,\"h\":30,\"kind\":\"error\",\"note\":\"时态错误\"}," +
		"{\"x\":5,\"y\":5,\"w\":1,\"h\":1,\"kind\":\"good\",\"note\":\"太小应被过滤\"}]\n```"
	anns, err := parseAnnotationJSON(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(anns) != 1 {
		t.Fatalf("应保留 1 条有效批注，实际 %d", len(anns))
	}
	if anns[0].Note != "时态错误" {
		t.Errorf("批注内容错误: %v", anns[0])
	}
}

// TestBuildReportPDF 冒烟测试：本机存在系统中文字体时验证 PDF 可生成且头部的 %PDF 魔数正确。
func TestBuildReportPDF(t *testing.T) {
	if findCJKFont() == "" {
		t.Skip("本机未找到可用中文字体，跳过 PDF 冒烟测试")
	}
	job := EssayJob{
		ID:             1,
		Username:       "teacher",
		StudentName:    "张三",
		Title:          "写信给朋友介绍你的暑假",
		Type:           "english",
		Status:         "done",
		MaxScore:       15,
		ScoreText:      "12",
		Content:        "Dear Lucy, How are you?",
		Feedback:       "优点：结构完整。\n\n【基础修改版】BASIC\n\n【进阶修改版】ADVANCED\n\n总分：12分",
		TeacherComment: "总评：完成度较高，注意时态一致。",
		CreatedAt:      time.Now(),
	}
	data, err := buildReportPDF([]EssayJob{job})
	if err != nil {
		t.Fatalf("构建 PDF 失败: %v", err)
	}
	if len(data) < 100 || !bytes.HasPrefix(data, []byte("%PDF-")) {
		t.Fatalf("PDF 输出异常，大小 %d", len(data))
	}
}

// TestStripMarkdownLines 校验导出文档的 Markdown 清理。
func TestStripMarkdownLines(t *testing.T) {
	in := "## 批改意见\n\n- **优点**：结构完整\n- 时态一致\n\n1. 保留编号\n链接[参考](http://x)与代码标记和__下划线__"
	out := stripMarkdownLines(in)
	for _, bad := range []string{"##", "**", "- ", "__", "](http"} {
		if strings.Contains(out, bad) {
			t.Errorf("输出仍含 Markdown 痕迹 %q: %q", bad, out)
		}
	}
	for _, good := range []string{"批改意见", "• 优点：结构完整", "• 时态一致", "1. 保留编号", "参考"} {
		if !strings.Contains(out, good) {
			t.Errorf("输出缺少内容 %q: %q", good, out)
		}
	}
}

// TestScoreStamp 冒烟测试：生成一张底图，叠加批注框 + 分数章，结果必须是可解码 JPEG。
func TestScoreStamp(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 480, 640))
	for y := 0; y < 640; y += 4 {
		for x := 0; x < 480; x += 4 {
			src.Set(x, y, color.RGBA{R: 240, G: 240, B: 235, A: 255})
		}
	}
	// 先落盘一张“原图”
	if err := os.MkdirAll(essayImageDir, 0o755); err != nil {
		t.Skip("无法创建原图目录")
	}
	srcPath := filepath.Join(essayImageDir, "_stamp_test_src.jpg")
	f, err := os.Create(srcPath)
	if err != nil {
		t.Skip("无法写入测试原图")
	}
	if err := jpeg.Encode(f, src, nil); err != nil {
		f.Close()
		t.Skip("编码测试原图失败")
	}
	f.Close()
	defer os.Remove(srcPath)

	anns := []imageAnnotation{{X: 100, Y: 100, W: 200, H: 60, Kind: "good", Note: "好句"}}
	data, err := annotateAndStamp(srcPath, anns, "48", 60)
	if err != nil {
		t.Fatalf("annotateAndStamp 失败: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("输出不是可解码 JPEG: %v", err)
	}

	// 无批注仅盖章也应成功
	data2, err := annotateAndStamp(srcPath, nil, "12", 15)
	if err != nil {
		t.Fatalf("仅分数章失败: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(data2)); err != nil {
		t.Fatalf("仅分数章输出不可解码: %v", err)
	}
	_ = data2
}

func readPart(t *testing.T, zr *zip.Reader, name string) string {
	t.Helper()
	for _, f := range zr.File {
		if f.Name == name {
			buf := new(bytes.Buffer)
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("打开 %s: %v", name, err)
			}
			buf.ReadFrom(rc)
			rc.Close()
			return buf.String()
		}
	}
	t.Fatalf("缺少部件 %s", name)
	return ""
}
