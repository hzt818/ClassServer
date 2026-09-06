// Copyright (c) 黄智韬. All rights reserved.

// 批改报告导出：docx（标准 OOXML，archive/zip 手写组装）+ PDF（go-pdf/fpdf + 系统中文字体）。
//
// 报告结构（单篇与批量共用，批量时每篇分页）：
//
//	品牌行 → 标题 → 信息区（姓名/类型/题目/得分）→ 名师点评 → 批改详情
//	→ AI 批注图 + 批注列表（有原图批注时）→ 学生原文（识别结果）→ 页脚
//
// docx 选择手写 OOXML 而非第三方库：报告是固定模板，最小 OOXML 集合
// （document.xml + rels + content types + media）即可稳定打开，零额外依赖。
package handlers

import (
	"archive/zip"
	"bytes"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"visualclassroom/classserver/database"

	"github.com/go-pdf/fpdf"
	"github.com/gofiber/fiber/v3"
)

var (
	reBoldMarkdown  = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reRevisionScore = regexp.MustCompile(`\n\s*总分\s*[：:]`)

	// Markdown 痕迹清理（导出文档中不出现任何 md 记号）
	reMdHeading = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s+`)
	reMdBullet  = regexp.MustCompile(`(?m)^(\s*)[-*+]\s+`)
	reMdLink    = regexp.MustCompile(`\[([^\]\n]*)\]\([^)\n]*\)`)
	reMdItalic  = regexp.MustCompile(`\*([^*\n]+)\*`)
	reMdUnder   = regexp.MustCompile(`_{2}([^_\n]+)_{2}`)
	reMdCode    = regexp.MustCompile("`" + `([^` + "`" + `\n]+)` + "`")
)

// stripMarkdownLines 全量去除 Markdown 痕迹：#/列表-/**加粗*/斜体/代码/链接；
// 无序列表项转为 "• "，其余记号直接剥离。PDF 导出与通用清洗使用。
func stripMarkdownLines(s string) string {
	s = reMdHeading.ReplaceAllString(s, "")
	s = reMdBullet.ReplaceAllString(s, "$1• ")
	s = reMdLink.ReplaceAllString(s, "$1")
	s = reBoldMarkdown.ReplaceAllString(s, "$1")
	s = reMdItalic.ReplaceAllString(s, "$1")
	s = reMdUnder.ReplaceAllString(s, "$1")
	s = reMdCode.ReplaceAllString(s, "$1")
	return strings.ReplaceAll(s, "**", "")
}

// cleanInlineMarkdown 单行内清洗（** 已被 docx 转成加粗运行，不再出现记号）。
func cleanInlineMarkdown(s string) string {
	s = reMdHeading.ReplaceAllString(s, "")
	s = reMdBullet.ReplaceAllString(s, "$1• ")
	s = reMdLink.ReplaceAllString(s, "$1")
	s = reMdItalic.ReplaceAllString(s, "$1")
	s = reMdUnder.ReplaceAllString(s, "$1")
	s = reMdCode.ReplaceAllString(s, "$1")
	return s
}

// ---------- 报告数据装配 ----------

type reportSegment struct {
	Text string
	Bold bool
}

type reportData struct {
	ID             int
	Name           string
	TypeLabel      string
	Title          string
	Score          string // "12 / 15 分"
	TeacherComment string
	ShortComment   string
	Main           string
	Basic          string
	Advanced       string
	UpgradeGuide   string // 升格指导（开头/中间/结尾 + 例句，中文作文）
	PolishedEssay  string // 全文升格范文（完整升华版）
	AnnotatedAbs   string // 批注图绝对路径（空=无）
	Annotations    []imageAnnotation
	Original       string
}

// parseBoldSegments 把 **加粗** 文本拆成片段（docx 用，PDF 直接剥离标记）。
func parseBoldSegments(text string) []reportSegment {
	segs := make([]reportSegment, 0, 4)
	last := 0
	for _, loc := range reBoldMarkdown.FindAllStringSubmatchIndex(text, -1) {
		if loc[0] > last {
			segs = append(segs, reportSegment{Text: text[last:loc[0]]})
		}
		segs = append(segs, reportSegment{Text: text[loc[2]:loc[3]], Bold: true})
		last = loc[1]
	}
	if last < len(text) {
		segs = append(segs, reportSegment{Text: text[last:]})
	}
	return segs
}

// prepareReportData 汇总单篇报告内容（批改详情自动拆出基础/进阶修改版）。
func prepareReportData(j EssayJob) reportData {
	name := strings.TrimSpace(j.StudentName)
	if name == "" {
		name = j.Username
	}
	label := essayTypeLabel[j.Type]
	if label == "" {
		label = "作文"
	}
	score := strings.TrimSpace(j.ScoreText)
	if score == "" || score == "--" || score == "错误" {
		score = "--"
	} else {
		score += " / " + strconv.Itoa(j.MaxScore) + " 分"
	}
	main, basic, advanced, guide, polished := splitRevisionSections(j.Feedback)

	r := reportData{
		ID:             j.ID,
		Name:           name,
		TypeLabel:      label,
		Title:          strings.TrimSpace(j.Title),
		Score:          score,
		TeacherComment: strings.TrimSpace(j.TeacherComment),
		ShortComment:   strings.TrimSpace(j.ShortComment),
		Main:           main,
		Basic:          basic,
		Advanced:       advanced,
		UpgradeGuide:   guide,
		PolishedEssay:  polished,
		Original:       strings.TrimSpace(j.Content),
	}
	if strings.TrimSpace(j.AnnotatedPath) != "" {
		p := filepath.Join(annotatedImageDir, filepath.Base(j.AnnotatedPath))
		if _, err := os.Stat(p); err == nil {
			r.AnnotatedAbs = p
		}
	}
	if strings.TrimSpace(j.Annotations) != "" {
		if anns, err := parseAnnotationJSON(j.Annotations); err == nil {
			r.Annotations = anns
		}
	}
	return r
}

// revisionTags 批改全文中的修改升格类章节标记（按输出顺序）。
var revisionTags = []string{"【基础修改版】", "【进阶修改版】", "【升格指导】", "【全文升格范文】"}

// splitRevisionSections 从批改全文拆出 主批改 / 基础修改版 / 进阶修改版 /
// 升格指导 / 全文升格范文。任一章节缺失返回空串；主批改为首个标记之前的内容。
func splitRevisionSections(feedback string) (main, basic, advanced, guide, polished string) {
	first := -1
	for _, t := range revisionTags {
		if i := strings.Index(feedback, t); i >= 0 && (first < 0 || i < first) {
			first = i
		}
	}
	if first < 0 {
		return strings.TrimSpace(feedback), "", "", "", ""
	}
	main = strings.TrimSpace(feedback[:first])
	rest := feedback[first:]
	if loc := reRevisionScore.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	section := func(tag string) string {
		i := strings.Index(rest, tag)
		if i < 0 {
			return ""
		}
		body := rest[i+len(tag):]
		end := len(body)
		for _, t2 := range revisionTags {
			if t2 == tag {
				continue
			}
			if j := strings.Index(body, t2); j >= 0 && j < end {
				end = j
			}
		}
		return strings.TrimSpace(body[:end])
	}
	return main,
		section("【基础修改版】"),
		section("【进阶修改版】"),
		section("【升格指导】"),
		section("【全文升格范文】")
}

// ---------- docx（OOXML） ----------

type docxBuilder struct {
	body     strings.Builder
	media    []docxMedia
	nextDraw int
}

type docxMedia struct {
	name string
	data []byte
}

func escXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}

const docxRFonts = `<w:rFonts w:ascii="Calibri" w:hAnsi="Calibri" w:eastAsia="微软雅黑"/>`

// addPara 输出一个段落（解析 **加粗**）；align: ""|"center"；indent 首行缩进（twip）。
func (d *docxBuilder) addPara(text, align, colorHex string, sizeHalf int, bold bool, indent int, lineH, after int) {
	pPr := "<w:pPr><w:spacing w:after=\"" + strconv.Itoa(after) + "\" w:line=\"" + strconv.Itoa(lineH) + "\" w:lineRule=\"auto\"/>"
	if indent > 0 {
		pPr += "<w:ind w:firstLine=\"" + strconv.Itoa(indent) + "\"/>"
	}
	if align != "" {
		pPr += "<w:jc w:val=\"" + align + "\"/>"
	}
	pPr += "</w:pPr>"
	d.body.WriteString("<w:p>" + pPr)
	for _, seg := range parseBoldSegments(text) {
		rPr := "<w:rPr>" + docxRFonts
		if bold || seg.Bold {
			rPr += "<w:b/>"
		}
		if colorHex != "" {
			rPr += "<w:color w:val=\"" + colorHex + "\"/>"
		}
		rPr += "<w:sz w:val=\"" + strconv.Itoa(sizeHalf) + "\"/><w:szCs w:val=\"" + strconv.Itoa(sizeHalf) + "\"/></w:rPr>"
		d.body.WriteString("<w:r>" + rPr + "<w:t xml:space=\"preserve\">" + escXML(cleanInlineMarkdown(seg.Text)) + "</w:t></w:r>")
	}
	d.body.WriteString("</w:p>")
}

// addLines 多行文本：每个 \n 一个段落（保留批改意见的行结构）。
func (d *docxBuilder) addLines(text, align, colorHex string, sizeHalf int, bold bool, indent int, lineH, after int) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	for _, p := range strings.Split(trimmed, "\n") {
		d.addPara(p, align, colorHex, sizeHalf, bold, indent, lineH, after)
	}
}

func (d *docxBuilder) addPageBreak() {
	d.body.WriteString(`<w:p><w:r><w:br w:type="page"/></w:r></w:p>`)
}

// addImage 插入图片（JPEG/PNG），按像素纵横比缩放，最大宽 16cm、最大高 20cm。
func (d *docxBuilder) addImage(data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return err
	}
	const maxW, maxH = 5760000, 7200000 // EMU（16cm / 20cm）
	w := maxW
	h := w * cfg.Height / cfg.Width
	if h > maxH {
		h = maxH
		w = h * cfg.Width / cfg.Height
	}
	d.nextDraw++
	relID := "rIdImg" + strconv.Itoa(d.nextDraw)
	ext := "jpeg"
	if isPNG(data) {
		ext = "png"
	}
	name := "media/image" + strconv.Itoa(d.nextDraw) + "." + ext
	d.media = append(d.media, docxMedia{name: name, data: data})

	docPrID := 100 + d.nextDraw
	d.body.WriteString(`<w:p><w:pPr><w:jc w:val="center"/></w:pPr><w:r><w:drawing>` +
		`<wp:inline distT="0" distB="0" distL="0" distR="0">` +
		`<wp:extent cx="` + strconv.Itoa(w) + `" cy="` + strconv.Itoa(h) + `"/>` +
		`<wp:effectExtent l="0" t="0" r="0" b="0"/>` +
		`<wp:docPr id="` + strconv.Itoa(docPrID) + `" name="Picture ` + strconv.Itoa(d.nextDraw) + `"/>` +
		`<wp:cNvGraphicFramePr><a:graphicFrameLocks xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" noChangeAspect="1"/></wp:cNvGraphicFramePr>` +
		`<a:graphic xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">` +
		`<a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture">` +
		`<pic:pic xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture">` +
		`<pic:nvPicPr><pic:cNvPr id="` + strconv.Itoa(docPrID) + `" name="` + name + `"/><pic:cNvPicPr/></pic:nvPicPr>` +
		`<pic:blipFill><a:blip r:embed="` + relID + `"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill>` +
		`<pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="` + strconv.Itoa(w) + `" cy="` + strconv.Itoa(h) + `"/></a:xfrm>` +
		`<a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr>` +
		`</pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>`)
	return nil
}

func isPNG(data []byte) bool {
	return len(data) > 4 && data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G'
}

func (d *docxBuilder) build() ([]byte, error) {
	doc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"><w:body>` +
		d.body.String() +
		`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1134" w:right="1134" w:bottom="1134" w:left="1134" w:header="709" w:footer="709" w:gutter="0"/></w:sectPr></w:body></w:document>`

	var rels strings.Builder
	rels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i, m := range d.media {
		rels.WriteString(`<Relationship Id="rIdImg` + strconv.Itoa(i+1) + `" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="` + m.name + `"/>`)
	}
	rels.WriteString(`</Relationships>`)

	contentTypes := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Default Extension="jpeg" ContentType="image/jpeg"/>` +
		`<Default Extension="png" ContentType="image/png"/>` +
		`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
		`</Types>`

	relsRoot := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>` +
		`</Relationships>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w := func(name, data string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = f.Write([]byte(data))
		return err
	}
	for _, part := range []struct{ name, data string }{
		{"[Content_Types].xml", contentTypes},
		{"_rels/.rels", relsRoot},
		{"word/document.xml", doc},
		{"word/_rels/document.xml.rels", rels.String()},
	} {
		if err := w(part.name, part.data); err != nil {
			return nil, err
		}
	}
	for _, m := range d.media {
		f, err := zw.Create("word/" + m.name)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(m.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeDocxReport 把一篇报告写入 docx 构建器。
func writeDocxReport(d *docxBuilder, r reportData) {
	d.addPara("MYCLASSROOM · 作文批改报告", "center", "B0B0B0", 15, false, 0, 240, 160)
	d.addPara("作文批改报告", "center", "1A1A1A", 40, false, 0, 240, 120)

	d.addPara("学生姓名："+r.Name+"　　作文类型："+r.TypeLabel+"　　得分："+r.Score, "", "1A1A1A", 21, false, 0, 240, 40)

	// 完整题目（材料与写作要求），保证报告自含全部题目信息
	if r.Title != "" {
		d.addPara("作文题目与材料", "", "C96442", 28, true, 0, 240, 40)
		d.addLines(r.Title, "", "1A1A1A", 22, false, 0, 300, 80)
	}

	if r.TeacherComment != "" {
		d.addPara("名师点评", "", "C96442", 28, true, 0, 240, 40)
		d.addLines(r.TeacherComment, "", "1A1A1A", 22, false, 0, 300, 80)
	}
	if r.ShortComment != "" {
		d.addPara("教师短评", "", "C96442", 28, true, 0, 240, 40)
		d.addLines(r.ShortComment, "", "1A1A1A", 22, false, 0, 300, 80)
	}

	d.addPara("批改详情", "", "C96442", 28, true, 0, 240, 40)
	if r.Main == "" {
		d.addPara("（暂无详细批改内容）", "", "8A8A8A", 22, false, 0, 300, 80)
	} else {
		d.addLines(r.Main, "", "1A1A1A", 22, false, 480, 300, 80)
	}

	if r.Basic != "" {
		d.addPara("基础修改版（参考）", "", "C96442", 28, true, 0, 240, 40)
		d.addLines(r.Basic, "", "1A1A1A", 22, false, 480, 300, 80)
	}
	if r.Advanced != "" {
		d.addPara("进阶修改版（升华参考）", "", "C96442", 28, true, 0, 240, 40)
		d.addLines(r.Advanced, "", "1A1A1A", 22, false, 480, 300, 80)
	}
	if r.UpgradeGuide != "" {
		d.addPara("升格指导（开头 / 中间 / 结尾）", "", "C96442", 28, true, 0, 240, 40)
		d.addLines(r.UpgradeGuide, "", "1A1A1A", 22, false, 480, 300, 80)
	}
	if r.PolishedEssay != "" {
		d.addPara("全文升格范文", "", "C96442", 28, true, 0, 240, 40)
		d.addLines(r.PolishedEssay, "", "1A1A1A", 22, false, 480, 300, 80)
	}

	if r.AnnotatedAbs != "" {
		if data, err := os.ReadFile(r.AnnotatedAbs); err == nil {
			d.addPara("AI 批注图（红=错误　绿=好句　橙=可提升）", "", "C96442", 28, true, 0, 240, 40)
			if err := d.addImage(data); err == nil {
				for i, a := range r.Annotations {
					kindName := map[string]string{"error": "错误", "good": "好句", "improve": "可提升"}[a.Kind]
					d.addPara("批注 "+strconv.Itoa(i+1)+"（"+kindName+"）："+a.Note, "", "1A1A1A", 21, false, 0, 240, 20)
				}
			}
		}
	}

	d.addPara("学生原文（识别结果）", "", "C96442", 28, true, 0, 240, 40)
	if r.Original == "" {
		d.addPara("（无识别原文）", "", "8A8A8A", 22, false, 0, 300, 80)
	} else {
		d.addLines(r.Original, "", "1A1A1A", 22, false, 480, 300, 80)
	}
	d.addPara("MYClassroom · 让每一堂课都活起来", "center", "B0B0B0", 15, false, 0, 240, 0)
}

// buildReportDocx 生成完整 docx 报告文件。
func buildReportDocx(jobs []EssayJob) ([]byte, error) {
	d := &docxBuilder{}
	for i, j := range jobs {
		if i > 0 {
			d.addPageBreak()
		}
		writeDocxReport(d, prepareReportData(j))
	}
	return d.build()
}

// ---------- PDF ----------

// findCJKFont 找一个可用的系统中文 TTF 字体（fpdf 的 UTF-8 支持需要 TTF，不支持 .ttc 集合）。
func findCJKFont() string {
	candidates := []string{
		os.Getenv("PDF_FONT_PATH"),
		`C:\Windows\Fonts\simhei.ttf`,
		`C:\Windows\Fonts\simkai.ttf`,
		`C:\Windows\Fonts\Deng.ttf`,
		`C:\Windows\Fonts\STZHONGS.TTF`,
		"/usr/share/fonts/truetype/wqy/wqy-microhei.ttf",
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

var (
	pdfTerra = struct{ R, G, B int }{201, 100, 66}
	pdfInk   = struct{ R, G, B int }{26, 26, 26}
	pdfGray  = struct{ R, G, B int }{138, 138, 138}
)

type pdfWriter struct {
	pdf *fpdf.Fpdf
}

// cjkCell 按宽度手动折行的中文文本（fpdf 对无空格中文不会自动换行），
// 渲染前清除全部 Markdown 痕迹。
func (p *pdfWriter) cjkCell(text string, size, lineH float64, col struct{ R, G, B int }, indentFirst bool) {
	p.pdf.SetFont("cjk", "", size)
	p.pdf.SetTextColor(col.R, col.G, col.B)
	pageW, _ := p.pdf.GetPageSize()
	ml, _, mr, _ := p.pdf.GetMargins()
	width := pageW - ml - mr
	plain := stripMarkdownLines(text)
	var line strings.Builder
	first := indentFirst
	flush := func() {
		if line.Len() == 0 {
			return
		}
		x := ml
		if first {
			x += 8
			first = false
		}
		p.pdf.SetX(x)
		p.pdf.CellFormat(width-(x-ml), lineH, line.String(), "", 1, "L", false, 0, "")
		line.Reset()
	}
	for _, r := range plain {
		line.WriteRune(r)
		if p.pdf.GetStringWidth(line.String()) >= width-10 {
			flush()
		}
	}
	flush()
}

func (p *pdfWriter) heading(text string) {
	p.pdf.Ln(2)
	p.pdf.SetTextColor(pdfTerra.R, pdfTerra.G, pdfTerra.B)
	p.pdf.SetFont("cjk", "", 13)
	p.pdf.CellFormat(0, 9, stripMarkdownLines(text), "", 1, "L", false, 0, "")
	p.pdf.SetDrawColor(pdfTerra.R, pdfTerra.G, pdfTerra.B)
	p.pdf.SetLineWidth(0.6)
	pdfX, pdfY := p.pdf.GetX(), p.pdf.GetY()
	p.pdf.Line(pdfX, pdfY, pdfX+14, pdfY)
	p.pdf.Ln(2.5)
}

func (p *pdfWriter) bodyText(text string, size float64, col struct{ R, G, B int }) {
	for _, para := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(para) == "" {
			continue
		}
		p.cjkCell(para, size, size*0.62, col, true)
	}
}

// buildReportPDF 生成完整 PDF 报告文件。
func buildReportPDF(jobs []EssayJob) ([]byte, error) {
	fontPath := findCJKFont()
	if fontPath == "" {
		return nil, errors.New("未找到系统中文字体（需要 TTF，如 C:\\Windows\\Fonts\\simhei.ttf）")
	}
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.SetAutoPageBreak(true, 15)
	pdf.AddUTF8Font("cjk", "", fontPath)
	p := &pdfWriter{pdf: pdf}

	for _, j := range jobs {
		// 每篇报告都显式开新页：必须在任何绘制调用之前 AddPage，
		// 否则 fpdf 处于"无页"状态时 CellFormat 的内容会写错缓冲区，产出损坏的 PDF
		pdf.AddPage()
		r := prepareReportData(j)
		// 页眉 + 标题
		pdf.SetTextColor(176, 176, 176)
		pdf.SetFont("cjk", "", 7.5)
		pdf.CellFormat(0, 6, "MYCLASSROOM · 作文批改报告", "", 1, "C", false, 0, "")
		pdf.SetTextColor(pdfInk.R, pdfInk.G, pdfInk.B)
		pdf.SetFont("cjk", "", 20)
		pdf.CellFormat(0, 14, "作文批改报告", "", 1, "C", false, 0, "")

		// 信息区
		pdf.SetFont("cjk", "", 10.5)
		pdf.SetTextColor(pdfGray.R, pdfGray.G, pdfGray.B)
		pdf.CellFormat(0, 6, "学生姓名："+r.Name+"　　作文类型："+r.TypeLabel+"　　得分："+r.Score, "", 1, "L", false, 0, "")
		pdf.Ln(1)
		if r.Title != "" {
			p.heading("作文题目与材料")
			p.bodyText(r.Title, 10, pdfInk)
		}

		if r.TeacherComment != "" {
			p.heading("名师点评")
			p.bodyText(r.TeacherComment, 10, pdfInk)
		}
		if r.ShortComment != "" {
			p.heading("教师短评")
			p.bodyText(r.ShortComment, 10, pdfInk)
		}

		p.heading("批改详情")
		if r.Main == "" {
			p.bodyText("（暂无详细批改内容）", 10, pdfGray)
		} else {
			p.bodyText(r.Main, 10, pdfInk)
		}

		if r.Basic != "" {
			p.heading("基础修改版（参考）")
			p.bodyText(r.Basic, 10, pdfInk)
		}
		if r.Advanced != "" {
			p.heading("进阶修改版（升华参考）")
			p.bodyText(r.Advanced, 10, pdfInk)
		}
		if r.UpgradeGuide != "" {
			p.heading("升格指导（开头 / 中间 / 结尾）")
			p.bodyText(r.UpgradeGuide, 10, pdfInk)
		}
		if r.PolishedEssay != "" {
			p.heading("全文升格范文")
			p.bodyText(r.PolishedEssay, 10, pdfInk)
		}

		if r.AnnotatedAbs != "" {
			pdf.AddPage()
			p.heading("AI 批注图（红=错误　绿=好句　橙=可提升）")
			if data, err := os.ReadFile(r.AnnotatedAbs); err == nil {
				if cfg, _, derr := image.DecodeConfig(bytes.NewReader(data)); derr == nil && cfg.Width > 0 {
					w := 165.0
					h := w * float64(cfg.Height) / float64(cfg.Width)
					if h > 190 {
						h = 190
						w = h * float64(cfg.Width) / float64(cfg.Height)
					}
					// 页面剩余高度不足时另起一页
					_, pageH := pdf.GetPageSize()
					_, _, _, mb := pdf.GetMargins()
					if pdf.GetY()+h > pageH-mb {
						pdf.AddPage()
					}
					y := pdf.GetY()
					pdf.SetX((210 - w) / 2)
					pdf.ImageOptions(r.AnnotatedAbs, (210-w)/2, y, w, h, false, fpdf.ImageOptions{ImageType: "JPEG"}, 0, "")
					pdf.SetY(y + h + 4)
				}
			}
			for i, a := range r.Annotations {
				kindName := map[string]string{"error": "错误", "good": "好句", "improve": "可提升"}[a.Kind]
				p.cjkCell("批注 "+strconv.Itoa(i+1)+"（"+kindName+"）："+a.Note, 9.5, 6, pdfInk, false)
			}
		}

		p.heading("学生原文（识别结果）")
		if r.Original == "" {
			p.bodyText("（无识别原文）", 10, pdfGray)
		} else {
			p.bodyText(r.Original, 10, pdfInk)
		}
		pdf.Ln(4)
		pdf.SetTextColor(176, 176, 176)
		pdf.SetFont("cjk", "", 7.5)
		pdf.CellFormat(0, 6, "MYClassroom · 让每一堂课都活起来", "", 1, "C", false, 0, "")
	}

	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ---------- 导出接口 ----------

// ExportEssayReports 导出批改报告：format=docx（默认）或 pdf。
// 请求体 {"ids":[...], "format":"docx|pdf"}；批量导出时每篇分页。
func ExportEssayReports(c fiber.Ctx) error {
	var req struct {
		IDs    []int  `json:"ids"`
		Format string `json:"format"`
	}
	if err := c.Bind().JSON(&req); err != nil || len(req.IDs) == 0 {
		return c.Status(400).JSON(fiber.Map{"error": "请选择要导出的任务"})
	}
	userID := c.Locals("userID").(int)
	role := c.Locals("role").(string)

	jobs := make([]EssayJob, 0, len(req.IDs))
	for _, id := range req.IDs {
		var j EssayJob
		var owner int
		err := database.Pool.QueryRow(c.Context(),
			"SELECT id, username, COALESCE(title,''), type, COALESCE(student_name,''), max_score, COALESCE(feedback,''), COALESCE(score_text,''), COALESCE(content,''), COALESCE(teacher_comment,''), COALESCE(short_comment,''), COALESCE(annotated_path,''), COALESCE(annotations,'') FROM essay_submissions WHERE id=$1 AND is_deleted=false", id).
			Scan(&j.ID, &j.Username, &j.Title, &j.Type, &j.StudentName, &j.MaxScore, &j.Feedback, &j.ScoreText, &j.Content, &j.TeacherComment, &j.ShortComment, &j.AnnotatedPath, &j.Annotations)
		if err != nil {
			continue // 任务不存在跳过
		}
		_ = database.Pool.QueryRow(c.Context(), "SELECT user_id FROM essay_submissions WHERE id=$1", id).Scan(&owner)
		if owner != userID && role != "teacher" {
			continue // 无权访问跳过
		}
		jobs = append(jobs, j)
	}
	if len(jobs) == 0 {
		return c.Status(404).JSON(fiber.Map{"error": "没有可导出的批改任务"})
	}

	var data []byte
	var err error
	format := strings.ToLower(strings.TrimSpace(req.Format))
	switch format {
	case "pdf":
		data, err = buildReportPDF(jobs)
	default:
		format = "docx"
		data, err = buildReportDocx(jobs)
	}
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "导出失败：" + err.Error()})
	}

	if format == "pdf" {
		c.Set("Content-Type", "application/pdf; charset=utf-8")
		c.Set("Content-Disposition", "attachment; filename=essay_reports.pdf")
	} else {
		c.Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
		c.Set("Content-Disposition", "attachment; filename=essay_reports.docx")
	}
	return c.Send(data)
}
