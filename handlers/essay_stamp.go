// Copyright (c) 黄智韬. All rights reserved.

// 阅卷系统风格的分数章：在批注图（原图 + AI 批注）右下角叠加一枚
// 手写风格的红色分数章——不规则双椭圆环模拟手绘圈，中间为分数数字，
// 下缘小字标注满分。数字用系统中文字体渲染（数字字形一致、平滑），
// 字体不可用时退化为像素数字。
package handlers

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	stampFontOnce   sync.Once
	stampParsedFont *opentype.Font
)

// loadStampFace 加载用于分数章数字的字体（仅首次解析，之后按字号生成 Face）。
func loadStampFace(size float64) (font.Face, bool) {
	stampFontOnce.Do(func() {
		path := findCJKFont()
		if path == "" {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		parsed, perr := opentype.Parse(data)
		if perr != nil {
			// 可能是 .ttc 集合：按集合解析取第一个字体
			if col, cerr := opentype.ParseCollection(data); cerr == nil {
				if single, ferr := col.Font(0); ferr == nil {
					parsed = single
				}
			}
		}
		stampParsedFont = parsed
	})
	if stampParsedFont == nil {
		return nil, false
	}
	face, err := opentype.NewFace(stampParsedFont, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, false
	}
	return face, true
}

// stampRed 分数章的红色（半透明红墨水感）。
func stampRed(a uint8) color.RGBA {
	return color.RGBA{R: 0xD9, G: 0x30, B: 0x25, A: a}
}

// fillCircle 在 img 上填充实心圆。
func fillCircle(img *image.RGBA, cx, cy, r float64, col color.RGBA) {
	x0, x1 := int(cx-r)-1, int(cx+r)+1
	y0, y1 := int(cy-r)-1, int(cy+r)+1
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			if dx*dx+dy*dy <= r*r {
				if x >= 0 && x < img.Bounds().Dx() && y >= 0 && y < img.Bounds().Dy() {
					img.SetRGBA(x, y, blendPixel(img.RGBAAt(x, y), col))
				}
			}
		}
	}
}

// blendPixel 把覆盖色按其 alpha 与底色混合（保留下方批注内容可见）。
func blendPixel(base color.RGBA, over color.RGBA) color.RGBA {
	if over.A == 0xFF {
		return over
	}
	a := float64(over.A) / 255
	r := float64(over.R)*a + float64(base.R)*(1-a)
	g := float64(over.G)*a + float64(base.G)*(1-a)
	b := float64(over.B)*a + float64(base.B)*(1-a)
	na := over.A + uint8((255-int(over.A))*int(base.A)/255)
	return color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: na}
}

// wobbleRing 画一圈带手绘抖动的椭圆环：半径随角度做小幅正弦扰动，
// 双圈错位叠加，观感接近教师手绘的分数圈。
func wobbleRing(img *image.RGBA, cx, cy, rx, ry, thickness float64, phase float64, col color.RGBA) {
	steps := int(math.Max(90, rx*6))
	degree := math.Pi * 2 / float64(steps)
	for i := 0; i < steps; i++ {
		theta := float64(i) * degree
		wob := 1 + 0.030*math.Sin(3*theta+phase) + 0.018*math.Sin(7*theta+phase*1.7)
		px := cx + rx*wob*math.Cos(theta)
		py := cy + ry*wob*math.Sin(theta)
		fillCircle(img, px, py, thickness/2, col)
	}
}

// drawScoreStamp 在批注图右下角叠加分数章；scoreText 无效时不绘制，返回是否绘制。
func drawScoreStamp(img *image.RGBA, scoreText string, maxScore int) bool {
	scoreStr := strings.TrimSpace(scoreText)
	if scoreStr == "" || scoreStr == "--" || scoreStr == "错误" {
		return false
	}
	scoreVal, err := strconv.ParseFloat(scoreStr, 64)
	if err != nil || scoreVal < 0 || maxScore <= 0 || scoreVal > float64(maxScore) {
		return false
	}
	scoreInt := int(math.Round(scoreVal))
	scoreStr = strconv.Itoa(scoreInt)

	W := img.Bounds().Dx()
	H := img.Bounds().Dy()
	if W < 120 || H < 120 {
		return false
	}
	R := float64(H) * 0.10
	if R < 38 {
		R = 38
	}
	if R > 110 {
		R = 110
	}
	margin := float64(H) * 0.045
	cx := float64(W) - R - margin
	cy := float64(H) - R - margin
	if cx < R || cy < R {
		cx = math.Max(R+2, cx)
		cy = math.Max(R+2, cy)
	}

	ink := stampRed(216)
	// 双圈：外圈略粗，内圈细且相位错开，形成手绘层次
	wobbleRing(img, cx, cy, R, R*0.86, math.Max(3, R*0.085), 0.8, ink)
	wobbleRing(img, cx, cy, R*0.90, R*0.76, math.Max(2, R*0.05), 2.9, ink)

	// 分数数字：大号分数 + 小号 "/满分"
	bigSize := R * 0.82
	smallSize := R * 0.30
	scoreLabel := scoreStr
	maxLabel := "/" + strconv.Itoa(maxScore)

	if face, ok := loadStampFace(bigSize); ok {
		drawStampText(img, face, scoreLabel, cx, cy+R*0.20, ink)
		if smallFace, ok2 := loadStampFace(smallSize); ok2 {
			drawStampText(img, smallFace, maxLabel, cx, cy+R*0.60, ink)
		}
		return true
	}

	// 字体不可用：退化为像素数字（3x5 位图放大）
	drawBitmapDigits(img, scoreLabel, cx, cy-R*0.05, R*0.20, ink)
	drawBitmapDigits(img, maxLabel, cx, cy+R*0.52, R*0.09, ink)
	return true
}

// drawStampText 用字体在 (cx, cyBaseline) 水平居中绘制一行文本（cyBaseline 为基线）。
func drawStampText(img *image.RGBA, face font.Face, text string, cx, cyBaseline float64, col color.RGBA) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
	}
	w := d.MeasureString(text)
	d.Dot = fixed.P(int(cx)-w.Ceil()/2, int(cyBaseline))
	d.DrawString(text)
}

// drawBitmapDigits 无字体时的像素数字回退（白色数字描红边观感由红色像素直接绘制）。
func drawBitmapDigits(img *image.RGBA, text string, cx, cy, scale float64, col color.RGBA) {
	digits := make([]int, 0, len(text))
	ok := true
	for _, ch := range text {
		switch {
		case ch >= '0' && ch <= '9':
			digits = append(digits, int(ch-'0'))
		case ch == '/':
			digits = append(digits, -1) // 斜杠
		default:
			ok = false
		}
	}
	if !ok || len(digits) == 0 {
		return
	}
	s := scale
	totalW := 0
	for range digits {
		totalW += int(4 * s)
	}
	x := cx - float64(totalW)/2
	for _, d := range digits {
		if d == -1 {
			// 斜杠：两像素宽的斜线
			for t := 0.0; t <= 1.0; t += 0.05 {
				px := x + (1-t)*2*s
				py := cy - 2.5*s + t*5*s
				fillCircle(img, px, py, s*0.55, col)
			}
			x += 4 * s
			continue
		}
		for row := 0; row < 5; row++ {
			for colI := 0; colI < 3; colI++ {
				if digitBitmap[d][row][colI] != '1' {
					continue
				}
				fillCircle(img, x+(float64(colI)+0.5)*s, cy-2.5*s+(float64(row)+0.5)*s, s*0.62, col)
			}
		}
		x += 4 * s
	}
}

// annotateAndStamp 组合输出：原图 + 批注框 + 分数章；anns 可为空（仅盖章）。
func annotateAndStamp(srcPath string, anns []imageAnnotation, scoreText string, maxScore int) ([]byte, error) {
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

	drawScoreStamp(img, scoreText, maxScore)

	buf := new(bytes.Buffer)
	if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
