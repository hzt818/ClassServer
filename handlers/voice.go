package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"strconv"
	"sync"
	"time"

	"visualclassroom/classserver/database"

	"github.com/gofiber/fiber/v3"
)

//  语音聊天

// VoiceParticipant 语音房间内的一名参与者
type VoiceParticipant struct {
	UserID   int    `json:"user_id"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Mic      bool   `json:"mic"`
	Cam      bool   `json:"cam"`
	ch       chan string
	lastSeen time.Time
}

// VoiceRoom 一个聊天室对应的语音房间状态
type VoiceRoom struct {
	mu           sync.Mutex
	ChatRoomID   int
	MessageID    int // 对应的 call_invite 消息 id
	Participants map[int]*VoiceParticipant
}

var (
	voiceRoomsMu sync.Mutex
	voiceRooms   = make(map[int]*VoiceRoom)
)

func getOrCreateVoiceRoom(roomID int) *VoiceRoom {
	voiceRoomsMu.Lock()
	defer voiceRoomsMu.Unlock()
	vr, ok := voiceRooms[roomID]
	if !ok {
		vr = &VoiceRoom{ChatRoomID: roomID, Participants: make(map[int]*VoiceParticipant)}
		voiceRooms[roomID] = vr
	}
	return vr
}

func getVoiceRoom(roomID int) *VoiceRoom {
	voiceRoomsMu.Lock()
	defer voiceRoomsMu.Unlock()
	return voiceRooms[roomID]
}

func removeVoiceRoomIfEmpty(roomID int) {
	voiceRoomsMu.Lock()
	defer voiceRoomsMu.Unlock()
	if vr, ok := voiceRooms[roomID]; ok {
		vr.mu.Lock()
		empty := len(vr.Participants) == 0
		vr.mu.Unlock()
		if empty {
			delete(voiceRooms, roomID)
		}
	}
}

// writeEvent 向某个参与者的信令channel非阻塞写入一条事件，处理不过来时直接丢弃。
func writeEvent(ch chan string, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	line := string(b) + "\n"
	select {
	case ch <- line:
	default:
	}
}

func (vr *VoiceRoom) broadcast(exclude int, v interface{}) {
	vr.mu.Lock()
	defer vr.mu.Unlock()
	for uid, p := range vr.Participants {
		if uid == exclude {
			continue
		}
		writeEvent(p.ch, v)
	}
}

func (vr *VoiceRoom) snapshot(exclude int) []VoiceParticipant {
	vr.mu.Lock()
	defer vr.mu.Unlock()
	list := make([]VoiceParticipant, 0, len(vr.Participants))
	for uid, p := range vr.Participants {
		if uid == exclude {
			continue
		}
		list = append(list, VoiceParticipant{UserID: p.UserID, Username: p.Username, FullName: p.FullName, Mic: p.Mic, Cam: p.Cam})
	}
	return list
}

func parseRoomID(c fiber.Ctx) (int, error) {
	return strconv.Atoi(c.Params("roomId"))
}

// VoiceInvite 发起/加入语音通话邀请：若房间已有进行中的通话气泡则直接复用，否则插入一条新的 call_invite 消息，前端据此渲染通话气泡。
func VoiceInvite(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	username := c.Locals("username").(string)
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效房间ID"})
	}

	var exists bool
	_ = database.Pool.QueryRow(c.Context(), "SELECT EXISTS(SELECT 1 FROM chat_rooms WHERE id=$1 AND is_deleted=false)", roomID).Scan(&exists)
	if !exists {
		return c.Status(404).JSON(fiber.Map{"error": "房间不存在"})
	}

	// 若已有进行中的通话，直接复用，不重复发气泡
	var existingID int
	_ = database.Pool.QueryRow(c.Context(),
		"SELECT id FROM messages WHERE room_id=$1 AND msg_type='call_invite' AND call_status='ongoing' ORDER BY id DESC LIMIT 1",
		roomID,
	).Scan(&existingID)

	vr := getOrCreateVoiceRoom(roomID)

	if existingID != 0 {
		vr.mu.Lock()
		vr.MessageID = existingID
		vr.mu.Unlock()
		return c.JSON(fiber.Map{"id": existingID, "room_id": roomID, "joined_existing": true})
	}

	var msgID int
	err = database.Pool.QueryRow(c.Context(),
		`INSERT INTO messages (user_id, username, content, room_id, msg_type, call_status)
		 VALUES ($1, $2, $3, $4, 'call_invite', 'ongoing') RETURNING id`,
		userID, username, username+" 发起了语音通话", roomID,
	).Scan(&msgID)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}

	vr.mu.Lock()
	vr.MessageID = msgID
	vr.mu.Unlock()

	return c.JSON(fiber.Map{"id": msgID, "room_id": roomID, "joined_existing": false})
}

// VoiceJoin 加入语音房间，返回当前已在房间内的其他成员列表
func VoiceJoin(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	username := c.Locals("username").(string)
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效房间ID"})
	}

	var req struct {
		Mic bool `json:"mic"`
		Cam bool `json:"cam"`
	}
	_ = c.Bind().JSON(&req)

	var fullName string
	_ = database.Pool.QueryRow(c.Context(), "SELECT full_name FROM users WHERE id=$1", userID).Scan(&fullName)
	if fullName == "" {
		fullName = username
	}

	vr := getOrCreateVoiceRoom(roomID)

	vr.mu.Lock()
	if vr.MessageID == 0 {
		vr.mu.Unlock()
		var mid int
		_ = database.Pool.QueryRow(c.Context(),
			"SELECT id FROM messages WHERE room_id=$1 AND msg_type='call_invite' AND call_status='ongoing' ORDER BY id DESC LIMIT 1",
			roomID,
		).Scan(&mid)
		vr.mu.Lock()
		vr.MessageID = mid
	}
	if existing, already := vr.Participants[userID]; already {
		// 同一个用户重复 join（比如页面跳转后的无感重连）：刷新心跳和设备状态，而不是报错拒绝
		existing.lastSeen = time.Now()
		existing.Mic = req.Mic
		existing.Cam = req.Cam
		vr.mu.Unlock()
		participants := vr.snapshot(userID)
		vr.broadcast(userID, fiber.Map{
			"type": "peer-state", "user_id": userID, "mic": req.Mic, "cam": req.Cam,
		})
		return c.JSON(fiber.Map{"participants": participants})
	}
	vr.Participants[userID] = &VoiceParticipant{
		UserID: userID, Username: username, FullName: fullName,
		Mic: req.Mic, Cam: req.Cam, ch: make(chan string, 64), lastSeen: time.Now(),
	}
	vr.mu.Unlock()

	participants := vr.snapshot(userID)
	vr.broadcast(userID, fiber.Map{
		"type": "peer-joined",
		"user": fiber.Map{"user_id": userID, "username": username, "full_name": fullName, "mic": req.Mic, "cam": req.Cam},
	})

	return c.JSON(fiber.Map{"participants": participants})
}

// VoiceStream 长连接信令通道：客户端 join 成功后立即订阅本接口接收后续事件
func VoiceStream(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效房间ID"})
	}

	vr := getVoiceRoom(roomID)
	if vr == nil {
		return c.Status(404).JSON(fiber.Map{"error": "语音房间不存在"})
	}
	vr.mu.Lock()
	p, ok := vr.Participants[userID]
	vr.mu.Unlock()
	if !ok {
		return c.Status(403).JSON(fiber.Map{"error": "请先加入语音房间"})
	}

	c.Set("Content-Type", "text/plain; charset=utf-8")
	c.Set("Transfer-Encoding", "chunked")
	c.Set("Cache-Control", "no-cache")
	c.Set("X-Accel-Buffering", "no")

	initPayload, _ := json.Marshal(fiber.Map{"type": "init", "participants": vr.snapshot(userID)})

	c.Response().SetBodyStreamWriter(func(w *bufio.Writer) {
		if _, err := w.WriteString(string(initPayload) + "\n"); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case msg, open := <-p.ch:
				if !open {
					return
				}
				if _, err := w.WriteString(msg); err != nil {
					return
				}
				if err := w.Flush(); err != nil {
					return
				}
			case <-ticker.C:
				if _, err := w.WriteString(`{"type":"ping"}` + "\n"); err != nil {
					return
				}
				if err := w.Flush(); err != nil {
					return
				}
			}
		}
	})
	return nil
}

// VoiceSignal 转发 WebRTC 信令（offer/answer/ice）给房间内指定的对方
func VoiceSignal(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效房间ID"})
	}
	var req struct {
		To         int             `json:"to"`
		SignalType string          `json:"signal_type"`
		Payload    json.RawMessage `json:"payload"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "参数错误"})
	}
	vr := getVoiceRoom(roomID)
	if vr == nil {
		return c.Status(404).JSON(fiber.Map{"error": "语音房间不存在"})
	}
	vr.mu.Lock()
	target, ok := vr.Participants[req.To]
	vr.mu.Unlock()
	if !ok {
		return c.Status(404).JSON(fiber.Map{"error": "对方已离开"})
	}
	writeEvent(target.ch, fiber.Map{
		"type": "signal", "from": userID, "signal_type": req.SignalType, "payload": req.Payload,
	})
	return c.JSON(fiber.Map{"ok": true})
}

// VoiceState 广播自己的麦克风/摄像头开关状态变化
func VoiceState(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效房间ID"})
	}
	var req struct {
		Mic *bool `json:"mic"`
		Cam *bool `json:"cam"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "参数错误"})
	}
	vr := getVoiceRoom(roomID)
	if vr == nil {
		return c.Status(404).JSON(fiber.Map{"error": "语音房间不存在"})
	}
	vr.mu.Lock()
	p, ok := vr.Participants[userID]
	if ok {
		p.lastSeen = time.Now()
		if req.Mic != nil {
			p.Mic = *req.Mic
		}
		if req.Cam != nil {
			p.Cam = *req.Cam
		}
	}
	vr.mu.Unlock()
	if !ok {
		return c.Status(404).JSON(fiber.Map{"error": "未加入该语音房间"})
	}
	vr.broadcast(userID, fiber.Map{"type": "peer-state", "user_id": userID, "mic": p.Mic, "cam": p.Cam})
	return c.JSON(fiber.Map{"ok": true})
}

// VoiceHeartbeat 通话期间客户端定期调用，防止被超时清理判定为掉线
func VoiceHeartbeat(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效房间ID"})
	}
	vr := getVoiceRoom(roomID)
	if vr == nil {
		return c.Status(404).JSON(fiber.Map{"error": "语音房间不存在"})
	}
	vr.mu.Lock()
	p, ok := vr.Participants[userID]
	if ok {
		p.lastSeen = time.Now()
	}
	vr.mu.Unlock()
	if !ok {
		return c.Status(404).JSON(fiber.Map{"error": "未加入该语音房间"})
	}
	return c.JSON(fiber.Map{"ok": true})
}

// VoiceLeave 离开语音房间；当房间人数归零时把对应的通话气泡标记为"已结束"。
func VoiceLeave(c fiber.Ctx) error {
	userID := c.Locals("userID").(int)
	roomID, err := parseRoomID(c)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "无效房间ID"})
	}
	vr := getVoiceRoom(roomID)
	if vr == nil {
		return c.JSON(fiber.Map{"ok": true})
	}

	vr.mu.Lock()
	p, ok := vr.Participants[userID]
	if ok {
		delete(vr.Participants, userID)
		close(p.ch)
	}
	remaining := len(vr.Participants)
	msgID := vr.MessageID
	vr.mu.Unlock()

	if ok {
		vr.broadcast(userID, fiber.Map{"type": "peer-left", "user_id": userID})
	}

	if remaining == 0 {
		var execErr error
		if msgID != 0 {
			_, execErr = database.Pool.Exec(c.Context(), "UPDATE messages SET call_status='ended' WHERE id=$1", msgID)
		} else {
			_, execErr = database.Pool.Exec(c.Context(),
				"UPDATE messages SET call_status='ended' WHERE room_id=$1 AND msg_type='call_invite' AND call_status='ongoing'", roomID)
		}
		if execErr != nil {
			log.Printf("🛑 更新语音通话结束状态失败: %v", execErr)
		}
		removeVoiceRoomIfEmpty(roomID)
	}

	return c.JSON(fiber.Map{"ok": true})
}

// forceLeaveVoice 复用 VoiceLeave 的核心逻辑，供心跳超时清理调用
func forceLeaveVoice(roomID, userID int) {
	vr := getVoiceRoom(roomID)
	if vr == nil {
		return
	}
	vr.mu.Lock()
	p, ok := vr.Participants[userID]
	if ok {
		delete(vr.Participants, userID)
		close(p.ch)
	}
	remaining := len(vr.Participants)
	msgID := vr.MessageID
	vr.mu.Unlock()

	if ok {
		vr.broadcast(userID, fiber.Map{"type": "peer-left", "user_id": userID})
	}

	if remaining == 0 {
		var execErr error
		if msgID != 0 {
			_, execErr = database.Pool.Exec(context.Background(), "UPDATE messages SET call_status='ended' WHERE id=$1", msgID)
		} else {
			_, execErr = database.Pool.Exec(context.Background(),
				"UPDATE messages SET call_status='ended' WHERE room_id=$1 AND msg_type='call_invite' AND call_status='ongoing'", roomID)
		}
		if execErr != nil {
			log.Printf("🛑 更新语音通话结束状态失败: %v", execErr)
		}
		removeVoiceRoomIfEmpty(roomID)
	}
}

// StartVoiceTimeoutSweeper 后台定期清理心跳超时
func StartVoiceTimeoutSweeper() {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			voiceRoomsMu.Lock()
			rooms := make([]*VoiceRoom, 0, len(voiceRooms))
			for _, vr := range voiceRooms {
				rooms = append(rooms, vr)
			}
			voiceRoomsMu.Unlock()

			for _, vr := range rooms {
				var stale []int
				vr.mu.Lock()
				for uid, p := range vr.Participants {
					if time.Since(p.lastSeen) > 60*time.Second {
						stale = append(stale, uid)
					}
				}
				vr.mu.Unlock()
				for _, uid := range stale {
					log.Printf("⏱️ 语音心跳超时，自动移出房间 %d 的用户 %d", vr.ChatRoomID, uid)
					forceLeaveVoice(vr.ChatRoomID, uid)
				}
			}
		}
	}()
}

// GetCurrentUser 返回当前登录用户的基本信息
func GetCurrentUser(c fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"user_id":  c.Locals("userID").(int),
		"username": c.Locals("username").(string),
		"role":     c.Locals("role").(string),
	})
}
