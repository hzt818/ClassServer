// Copyright (c) 黄智韬. All rights reserved.

// 课堂传屏：教师浏览器采集屏幕（getDisplayMedia）→ JPEG 帧上传 → 服务端
// 双画质（原始高清 + 服务端缩放的 720p 流畅）→ 学生端以 MJPEG
// （multipart/x-mixed-replace）长连接拉流。
//
// 60 人规模的流畅度策略（教室局域网）：
//  1. 变化帧门控：教师端对画面块采样签名，画面未变不上传；服务端再按 SHA-256 去重，
//     静止的课件/板书帧成本趋近于 0，只有翻页/书写时产生流量；
//  2. 双画质分摊带宽：手机等小屏默认拉 720p"流畅"流，电脑可选"高清"原始流，
//     同样的帧服务端只存两份，逐连接分发给各自画质；
//  3. 每连接独立节流：MJPEG 流内逐帧 flush，断开即释放。
//
// 录屏：教师端 MediaRecorder（系统声音 + 可选麦克风混音）分片上传，
// 服务端按序号落盘、结束时拼接为 webm 存档，可下载/删除。
//
// 录屏文件的读写全部经由 os.Root（内核级目录限定）：任何含 .. / 绝对路径 /
// 保留名的文件名都会被拒绝，文件操作不可能越出录屏目录；文件名再叠加
// ^rec_[0-9]+$ 白名单与 filepath.IsLocal 双重校验。
package handlers

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
)

const castRecordDir = "./data/recordings"

// recIDPattern 录屏 ID 白名单：杜绝路径拼接类风险。
var recIDPattern = regexp.MustCompile(`^rec_[0-9]+$`)

func safeRecordID(s string) string {
	s = strings.TrimSpace(s)
	if !recIDPattern.MatchString(s) {
		return ""
	}
	return s
}

// recordFileName 校验录屏 ID 后给出限定在录屏目录内的相对文件名。
func recordFileName(id, suffix string) (string, error) {
	if id == "" || !recIDPattern.MatchString(id) || !filepath.IsLocal(id+suffix) {
		return "", fmt.Errorf("非法录屏文件名")
	}
	return id + suffix, nil
}

// openCastRoot 打开录屏目录的 os.Root（所有文件操作限定在该目录内）。
func openCastRoot() (*os.Root, error) {
	if err := os.MkdirAll(castRecordDir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenRoot(castRecordDir)
}

// castColor 构造不透明颜色。
func castColor(r, g, b uint8) color.RGBA {
	return color.RGBA{R: r, G: g, B: b, A: 0xFF}
}

var cast = castState{}

type castState struct {
	mu      sync.Mutex
	live    bool
	started time.Time
	version int64 // 每次有效帧 +1
	desktop []byte
	mobile  []byte
	sha     string
	// viewers 当前观看连接数；lastSeen 最后收到帧的毫秒时间戳
	viewers  atomic.Int64
	lastSeen atomic.Int64
}

func init() {
	if err := os.MkdirAll(castRecordDir, 0o755); err != nil {
		log.Printf("创建录屏目录失败: %v", err)
	}
}

// ---------- 教师端：帧上传 ----------

// CastFrame 接收教师端上传的一帧 JPEG；画面未变化时直接丢弃（不产生分发流量）。
func CastFrame(c fiber.Ctx) error {
	body := c.Body()
	if len(body) < 4 || body[0] != 0xFF || body[1] != 0xD8 {
		return c.Status(400).JSON(fiber.Map{"error": "无效的 JPEG 帧"})
	}
	sum := sha256.Sum256(body)
	shaStr := hex.EncodeToString(sum[:8])

	cast.mu.Lock()
	defer cast.mu.Unlock()
	if cast.sha == shaStr {
		cast.lastSeen.Store(time.Now().UnixMilli())
		return c.JSON(fiber.Map{"ok": true, "changed": false})
	}
	mobile := downscaleJPEG(body, 720, 50)
	if !cast.live {
		cast.live = true
		cast.started = time.Now()
	}
	cast.desktop = body
	if len(mobile) > 0 {
		cast.mobile = mobile
	} else {
		cast.mobile = body // 缩放失败时共用原始帧，保证不中断
	}
	cast.sha = shaStr
	cast.version++
	cast.lastSeen.Store(time.Now().UnixMilli())
	return c.JSON(fiber.Map{"ok": true, "changed": true, "version": cast.version})
}

// CastStop 教师停止传屏：清空状态，学生端回到等待页。
func CastStop(c fiber.Ctx) error {
	cast.mu.Lock()
	cast.live = false
	cast.desktop = nil
	cast.mobile = nil
	cast.sha = ""
	cast.version++
	cast.mu.Unlock()
	return c.JSON(fiber.Map{"ok": true})
}

// CastStatus 传屏状态（学生端轮询 / 教师端面板展示）。
func CastStatus(c fiber.Ctx) error {
	cast.mu.Lock()
	live := cast.live
	started := cast.started
	cast.mu.Unlock()
	return c.JSON(fiber.Map{
		"live":      live,
		"viewers":   cast.viewers.Load(),
		"started":   started,
		"recording": recordingActive.Load(),
	})
}

// ---------- 学生端：MJPEG 拉流 ----------

const mjpegBoundary = "myclassroomcast"

// CastLive MJPEG 直播流：multipart/x-mixed-replace，浏览器可直接用 <img> 渲染。
// tier=mobile（默认）拉 720p 流畅流，tier=desktop 拉原始高清流。
func CastLive(c fiber.Ctx) error {
	tier := c.Query("tier", "mobile")
	if tier != "desktop" && tier != "mobile" {
		tier = "mobile"
	}
	cast.viewers.Add(1)
	defer cast.viewers.Add(-1)

	c.Response().Header.SetContentType("multipart/x-mixed-replace; boundary=" + mjpegBoundary)
	c.Response().Header.Set("Cache-Control", "no-cache, no-store")
	c.Response().Header.Set("Connection", "close")
	c.Response().SetBodyStreamWriter(func(w *bufio.Writer) {
		lastVersion := int64(-1)
		lastWrite := time.Now()
		for {
			var frame []byte
			var changed bool
			cast.mu.Lock()
			live := cast.live
			if live {
				if cast.version != lastVersion {
					changed = true
					lastVersion = cast.version
				}
				if tier == "desktop" {
					frame = cast.desktop
				} else {
					frame = cast.mobile
				}
			}
			cast.mu.Unlock()

			if !live {
				return // 老师停止传屏：结束流，学生端轮询状态后回到等待页
			}
			// 帧变化立即写；画面静止时每 8 秒重发当前帧作为保活（兼探活断开）
			if changed || time.Since(lastWrite) >= 8*time.Second {
				if len(frame) == 0 {
					time.Sleep(100 * time.Millisecond)
					continue
				}
				w.WriteString("--" + mjpegBoundary + "\r\n")
				w.WriteString("Content-Type: image/jpeg\r\n")
				w.WriteString("Content-Length: " + strconv.Itoa(len(frame)) + "\r\n\r\n")
				if _, err := w.Write(frame); err != nil {
					return // 学生断开
				}
				w.WriteString("\r\n")
				if err := w.Flush(); err != nil {
					return
				}
				lastWrite = time.Now()
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return nil
}

// downscaleJPEG 纯标准库的盒滤波缩放：高度超过 maxH 时等比缩到 maxH 再编码。
func downscaleJPEG(src []byte, maxH, quality int) []byte {
	img, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		return nil
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if h <= maxH || w <= 0 {
		return nil
	}
	nh := maxH
	nw := int(float64(w) * float64(maxH) / float64(h))
	if nw < 1 {
		return nil
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	sx := float64(w) / float64(nw)
	sy := float64(h) / float64(nh)
	for dy := 0; dy < nh; dy++ {
		y0 := int(float64(dy) * sy)
		y1 := int(float64(dy+1) * sy)
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < nw; dx++ {
			x0 := int(float64(dx) * sx)
			x1 := int(float64(dx+1) * sx)
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, cnt uint32
			for yy := y0; yy < y1 && yy < h; yy++ {
				for xx := x0; xx < x1 && xx < w; xx++ {
					pr, pg, pb, _ := img.At(b.Min.X+xx, b.Min.Y+yy).RGBA()
					r += pr >> 8
					g += pg >> 8
					bl += pb >> 8
					cnt++
				}
			}
			if cnt == 0 {
				continue
			}
			dst.SetRGBA(dx, dy, castColor(uint8(r/cnt), uint8(g/cnt), uint8(bl/cnt)))
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
		return nil
	}
	return buf.Bytes()
}

// ---------- 录屏（全部经由 os.Root 限定在录屏目录内） ----------

var recordingActive atomic.Bool

type recordingMeta struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	StartedAt int64  `json:"started_at"`
	Duration  int64  `json:"duration_ms"`
	Size      int64  `json:"size"`
}

// listCastEntries 列出录屏目录中的条目（经由 os.Root 限定）。
func listCastEntries(root *os.Root) ([]os.DirEntry, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.ReadDir(-1)
}

// recordPartPaths 列出某录屏 ID 的全部分片。
func recordPartPaths(id string) ([]string, error) {
	root, err := openCastRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := listCastEntries(root)
	if err != nil {
		return nil, err
	}
	prefix := id + ".part"
	names := make([]string, 0, 8)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// CastRecordStart 开始一次录屏，返回录屏 ID。
func CastRecordStart(c fiber.Ctx) error {
	id := fmt.Sprintf("rec_%d", time.Now().UnixNano())
	meta := recordingMeta{ID: id, StartedAt: time.Now().UnixMilli(), Name: "课堂录屏 " + time.Now().Format("01-02 15:04")}
	if err := writeRecordingMeta(meta); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "创建录屏失败"})
	}
	recordingActive.Store(true)
	return c.JSON(fiber.Map{"ok": true, "id": id})
}

// CastRecordChunk 追加一个录屏分片（MediaRecorder timeslice 输出）。
func CastRecordChunk(c fiber.Ctx) error {
	id := safeRecordID(c.Query("id"))
	if id == "" {
		return c.Status(400).JSON(fiber.Map{"error": "无效录屏 ID"})
	}
	body := c.Body()
	if len(body) == 0 {
		return c.JSON(fiber.Map{"ok": true})
	}
	seq, err := strconv.ParseUint(c.Query("seq", "0"), 10, 32)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效分片序号"})
	}
	name, err := recordFileName(id, fmt.Sprintf(".part%010d", seq))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效录屏路径"})
	}
	root, rerr := openCastRoot()
	if rerr != nil {
		return c.Status(500).JSON(fiber.Map{"error": "打开录屏目录失败"})
	}
	defer root.Close()
	f, err := root.Create(name)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "写入分片失败"})
	}
	_, werr := f.Write(body)
	cerr := f.Close()
	if werr != nil {
		return c.Status(500).JSON(fiber.Map{"error": "写入分片失败"})
	}
	_ = cerr
	return c.JSON(fiber.Map{"ok": true})
}

// CastRecordFinish 结束录屏：按序拼接分片为 webm 存档。
func CastRecordFinish(c fiber.Ctx) error {
	id := safeRecordID(c.Query("id"))
	if id == "" {
		return c.Status(400).JSON(fiber.Map{"error": "无效录屏 ID"})
	}
	root, rerr := openCastRoot()
	if rerr != nil {
		return c.Status(500).JSON(fiber.Map{"error": "打开录屏目录失败"})
	}
	defer root.Close()

	parts, err := recordPartPaths(id)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "读取分片失败"})
	}
	var out bytes.Buffer
	for _, name := range parts {
		f, err := root.Open(name)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			continue
		}
		out.Write(data)
		root.Remove(name)
	}
	if out.Len() == 0 {
		recordingActive.Store(false)
		return c.Status(400).JSON(fiber.Map{"error": "没有录到任何内容"})
	}
	finalName, err := recordFileName(id, ".webm")
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效录屏路径"})
	}
	f, err := root.Create(finalName)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "保存录屏失败"})
	}
	_, werr := f.Write(out.Bytes())
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return c.Status(500).JSON(fiber.Map{"error": "保存录屏失败"})
	}
	meta := readRecordingMeta(id)
	if meta != nil {
		meta.Size = int64(out.Len())
		meta.Duration = time.Now().UnixMilli() - meta.StartedAt
		_ = writeRecordingMeta(*meta)
	}
	recordingActive.Store(false)
	return c.JSON(fiber.Map{"ok": true, "size": out.Len()})
}

// CastRecordings 录屏列表。
func CastRecordings(c fiber.Ctx) error {
	root, err := openCastRoot()
	if err != nil {
		return c.JSON([]recordingMeta{})
	}
	defer root.Close()
	entries, err := listCastEntries(root)
	if err != nil {
		return c.JSON([]recordingMeta{})
	}
	list := make([]recordingMeta, 0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if m := readRecordingMeta(strings.TrimSuffix(e.Name(), ".json")); m != nil {
			list = append(list, *m)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].StartedAt > list[j].StartedAt })
	return c.JSON(list)
}

// CastRecordingDownload 下载录屏文件。
func CastRecordingDownload(c fiber.Ctx) error {
	id := safeRecordID(c.Params("id"))
	if id == "" {
		return c.Status(400).JSON(fiber.Map{"error": "无效录屏 ID"})
	}
	name, err := recordFileName(id, ".webm")
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效录屏路径"})
	}
	root, rerr := openCastRoot()
	if rerr != nil {
		return c.Status(500).JSON(fiber.Map{"error": "打开录屏目录失败"})
	}
	defer root.Close()
	if _, err := root.Stat(name); err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "录屏不存在"})
	}
	absPath := filepath.Join(castRecordDir, name)
	c.Set("Content-Type", "video/webm")
	c.Set("Content-Disposition", "attachment; filename=\""+id+".webm\"")
	return c.SendFile(absPath)
}

// CastRecordingDelete 删除录屏。
func CastRecordingDelete(c fiber.Ctx) error {
	id := safeRecordID(c.Params("id"))
	if id == "" {
		return c.Status(400).JSON(fiber.Map{"error": "无效录屏 ID"})
	}
	webmName, err := recordFileName(id, ".webm")
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效录屏路径"})
	}
	root, rerr := openCastRoot()
	if rerr != nil {
		return c.Status(500).JSON(fiber.Map{"error": "打开录屏目录失败"})
	}
	defer root.Close()
	root.Remove(webmName)
	jsonName, _ := recordFileName(id, ".json")
	if jsonName != "" {
		root.Remove(jsonName)
	}
	if parts, perr := recordPartPaths(id); perr == nil {
		for _, name := range parts {
			root.Remove(name)
		}
	}
	return c.JSON(fiber.Map{"ok": true})
}

func writeRecordingMeta(m recordingMeta) error {
	if _, err := recordFileName(m.ID, ".json"); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	root, err := openCastRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	name, err := recordFileName(m.ID, ".json")
	if err != nil {
		return err
	}
	f, err := root.Create(name)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func readRecordingMeta(id string) *recordingMeta {
	name, err := recordFileName(id, ".json")
	if err != nil {
		return nil
	}
	root, err := openCastRoot()
	if err != nil {
		return nil
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return nil
	}
	var m recordingMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return &m
}
