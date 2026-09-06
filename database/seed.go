package database

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// SeedDemoAccounts 由 main 传入（app.seed_demo_accounts，默认 false），
// 为 true 时才预置 admin01-10 / u001-100 调试账户。
var SeedDemoAccounts bool

// SeedUsers 创建全部表结构并写入种子数据（SQLite 方言）。
// 全部列直接写入 CREATE TABLE，历史 PG 库的增量 ALTER 垫片不再需要。
// seedDemoAccounts=true 时才预置调试账户（admin01-10 / u001-100），默认不创建。
func SeedUsers(seedDemoAccounts bool) error {
	SeedDemoAccounts = seedDemoAccounts
	// ---------- 1. users ----------
	createUsersSQL := `
    CREATE TABLE IF NOT EXISTS users (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        username VARCHAR(50) UNIQUE NOT NULL,
        password_hash VARCHAR(255) NOT NULL,
        role VARCHAR(20) NOT NULL,
        full_name VARCHAR(100),
        avatar VARCHAR(255),
        last_login TIMESTAMP,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        is_certified BOOLEAN NOT NULL DEFAULT 1,
        real_name VARCHAR(100),
        alipay_user_id VARCHAR(64)
    );`
	if _, err := Pool.Exec(context.Background(), createUsersSQL); err != nil {
		return err
	}
	// 旧库补列：is_certified 默认 1（历史内置账号视为已认可），自助注册的教师账号显式写 0 待认证
	if err := ensureColumn("users", "is_certified BOOLEAN NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := ensureColumn("users", "real_name VARCHAR(100)"); err != nil {
		return err
	}
	if err := ensureColumn("users", "alipay_user_id VARCHAR(64)"); err != nil {
		return err
	}

	// ---------- 2. chat_rooms ----------
	createRoomsSQL := `
    CREATE TABLE IF NOT EXISTS chat_rooms (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        name VARCHAR(100) NOT NULL,
        created_by INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        is_deleted BOOLEAN DEFAULT FALSE,
        deleted_at TIMESTAMP
    );`
	if _, err := Pool.Exec(context.Background(), createRoomsSQL); err != nil {
		return err
	}

	// ---------- 3. messages ----------
	createMessagesSQL := `
    CREATE TABLE IF NOT EXISTS messages (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        username VARCHAR(50) NOT NULL,
        content TEXT NOT NULL,
        room_id INTEGER NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        is_recalled BOOLEAN DEFAULT FALSE,
        recalled_at TIMESTAMP,
        is_streaming BOOLEAN DEFAULT FALSE,
        msg_type VARCHAR(20) NOT NULL DEFAULT 'text',
        call_status VARCHAR(20),
        reply_to_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
        quoted_message_ids TEXT
    );`
	if _, err := Pool.Exec(context.Background(), createMessagesSQL); err != nil {
		return err
	}
	if _, err := Pool.Exec(context.Background(),
		"CREATE INDEX IF NOT EXISTS idx_messages_reply_to ON messages (reply_to_id)"); err != nil {
		return err
	}

	// ---------- 4. user_hidden_rooms ----------
	createHiddenSQL := `
    CREATE TABLE IF NOT EXISTS user_hidden_rooms (
        user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        room_id INTEGER NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
        hidden_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        PRIMARY KEY (user_id, room_id)
    );`
	if _, err := Pool.Exec(context.Background(), createHiddenSQL); err != nil {
		return err
	}

	// ---------- 5. 种子用户（仅调试模式：10 名教师 + 100 名学生，随机初始密码） ----------
	// 默认不创建任何调试账户；需要批量测试账号时在 config.properties 打开
	// app.seed_demo_accounts=true 后重启，初始密码清单会写入 data/initial_accounts.txt。
	if !SeedDemoAccounts {
		log.Println("✅ 跳过调试账户预置（app.seed_demo_accounts=false）")
	} else {
		seedUsers := make(map[string]string)
		for i := 1; i <= 10; i++ {
			username := "admin" + padZero(i, 2)
			seedUsers[username] = "teacher"
		}
		for i := 1; i <= 100; i++ {
			username := "u" + padZero(i, 3)
			seedUsers[username] = "student"
		}

		created := make(map[string]string, len(seedUsers)) // 仅记录本次新建账号的初始密码（已存在的保持原密码）
		for username, role := range seedUsers {
			plainPwd := randomPassword()
			hashed, err := bcrypt.GenerateFromPassword([]byte(plainPwd), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			insertSQL := `
        INSERT INTO users (username, password_hash, role, full_name)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (username) DO NOTHING;`
			res, err := Pool.Exec(context.Background(), insertSQL, username, string(hashed), role, username)
			if err != nil {
				return err
			}
			if res.RowsAffected() > 0 {
				created[username] = plainPwd
			}
		}
		if err := writeInitialAccounts(created); err != nil {
			log.Printf("⚠️  初始账号清单写入失败（不影响服务运行）: %v", err)
		}
	}

	// ---------- 6. 默认公共聊天室 ----------
	// 创建者优先取 admin01（调试账户开启时），否则取第一个教师账户，再回退第一个用户
	var adminID int
	err := Pool.QueryRow(context.Background(), "SELECT id FROM users WHERE username='admin01'").Scan(&adminID)
	if err != nil {
		err = Pool.QueryRow(context.Background(), "SELECT id FROM users WHERE role='teacher' ORDER BY id LIMIT 1").Scan(&adminID)
	}
	if err != nil {
		err = Pool.QueryRow(context.Background(), "SELECT id FROM users ORDER BY id LIMIT 1").Scan(&adminID)
	}
	if err != nil {
		log.Println("警告：库中尚无任何用户，公共聊天室将在创建首个用户后由该用户补建")
		adminID = 0
	}
	defaultRoomSQL := `
    INSERT INTO chat_rooms (name, created_by)
    SELECT '公共聊天室', $1
    WHERE NOT EXISTS (SELECT 1 FROM chat_rooms);`
	if adminID > 0 {
		if _, err := Pool.Exec(context.Background(), defaultRoomSQL, adminID); err != nil {
			return err
		}
	}

	// ---------- 7. 作文批改 ----------
	createSubmissionsSQL := `
	CREATE TABLE IF NOT EXISTS essay_submissions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		username VARCHAR(50) NOT NULL,
		title TEXT,
		content TEXT NOT NULL,
		type VARCHAR(20) NOT NULL,
		status VARCHAR(20) NOT NULL DEFAULT 'done',
		feedback TEXT NOT NULL DEFAULT '',
		score_text VARCHAR(20),
		max_score INTEGER NOT NULL DEFAULT 100,
		student_name VARCHAR(100),
		is_streaming BOOLEAN NOT NULL DEFAULT FALSE,
		teacher_edited BOOLEAN NOT NULL DEFAULT FALSE,
		is_deleted BOOLEAN NOT NULL DEFAULT FALSE,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		image_path VARCHAR(255),
		teacher_comment TEXT,
		short_comment TEXT,
		annotated_path VARCHAR(255),
		annotations TEXT,
		excellent BOOLEAN NOT NULL DEFAULT FALSE,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := Pool.Exec(context.Background(), createSubmissionsSQL); err != nil {
		return err
	}
	// 历史库增量垫片：CREATE TABLE IF NOT EXISTS 不会补新列，逐列尝试加，
	// 已存在的列会报 "duplicate column name" 错误，忽略即可。
	for _, col := range []string{
		"ALTER TABLE essay_submissions ADD COLUMN teacher_comment TEXT",
		"ALTER TABLE essay_submissions ADD COLUMN short_comment TEXT",
		"ALTER TABLE essay_submissions ADD COLUMN annotated_path VARCHAR(255)",
		"ALTER TABLE essay_submissions ADD COLUMN annotations TEXT",
		"ALTER TABLE essay_submissions ADD COLUMN excellent BOOLEAN NOT NULL DEFAULT FALSE",
	} {
		_, _ = Pool.Exec(context.Background(), col)
	}
	if _, err := Pool.Exec(context.Background(),
		"CREATE INDEX IF NOT EXISTS idx_essay_submissions_user ON essay_submissions (user_id, is_deleted, created_at DESC)"); err != nil {
		return err
	}

	createDraftsSQL := `
	CREATE TABLE IF NOT EXISTS essay_drafts (
	    id INTEGER PRIMARY KEY AUTOINCREMENT,
	    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	    kind VARCHAR(10) NOT NULL,
	    content TEXT NOT NULL,
	    student_name VARCHAR(100),
	    seq INTEGER NOT NULL DEFAULT 0,
	    image_path VARCHAR(255),
	    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := Pool.Exec(context.Background(), createDraftsSQL); err != nil {
		return err
	}
	if _, err := Pool.Exec(context.Background(),
		"CREATE INDEX IF NOT EXISTS idx_essay_drafts_user ON essay_drafts (user_id, seq)"); err != nil {
		return err
	}

	// ---------- 8. message_reactions ----------
	createReactionsSQL := `
	CREATE TABLE IF NOT EXISTS message_reactions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		reaction_type VARCHAR(16) NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		UNIQUE (message_id, user_id, reaction_type)
	);`
	if _, err := Pool.Exec(context.Background(), createReactionsSQL); err != nil {
		return err
	}
	if _, err := Pool.Exec(context.Background(),
		"CREATE INDEX IF NOT EXISTS idx_message_reactions_message ON message_reactions (message_id)"); err != nil {
		return err
	}

	// ---------- 9. message_mentions ----------
	createMentionsSQL := `
	CREATE TABLE IF NOT EXISTS message_mentions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		room_id INTEGER NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
		mentioner_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		mentioner_username VARCHAR(50) NOT NULL,
		mentioned_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		content_snippet TEXT,
		seen BOOLEAN NOT NULL DEFAULT FALSE,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := Pool.Exec(context.Background(), createMentionsSQL); err != nil {
		return err
	}
	if _, err := Pool.Exec(context.Background(),
		"CREATE INDEX IF NOT EXISTS idx_message_mentions_pending ON message_mentions (mentioned_user_id, seen)"); err != nil {
		return err
	}
	if _, err := Pool.Exec(context.Background(),
		"CREATE INDEX IF NOT EXISTS idx_message_mentions_room ON message_mentions (room_id, mentioned_user_id, seen)"); err != nil {
		return err
	}

	// ---------- 10. 聊天时段规则 ----------
	createChatTimeRulesSQL := `
	CREATE TABLE IF NOT EXISTS chat_time_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		mode VARCHAR(10) NOT NULL CHECK (mode IN ('weekday', 'date')),
		day_of_week INTEGER CHECK (day_of_week BETWEEN 1 AND 7),
		specific_date TEXT,
		start_time TEXT NOT NULL,
		end_time TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := Pool.Exec(context.Background(), createChatTimeRulesSQL); err != nil {
		return err
	}
	if _, err := Pool.Exec(context.Background(),
		"CREATE INDEX IF NOT EXISTS idx_chat_time_rules_mode ON chat_time_rules (mode)"); err != nil {
		return err
	}

	// ---------- 11. chat_files ----------
	createFilesSQL := `
	CREATE TABLE IF NOT EXISTS chat_files (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		uploader_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		room_id INTEGER NOT NULL REFERENCES chat_rooms(id) ON DELETE CASCADE,
		filename VARCHAR(255) NOT NULL,
		filepath VARCHAR(512) NOT NULL,
		filesize BIGINT NOT NULL,
		access_type VARCHAR(20) NOT NULL DEFAULT 'public',
		target_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
		is_deleted BOOLEAN DEFAULT FALSE,
		deleted_at TIMESTAMP,
		uploaded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := Pool.Exec(context.Background(), createFilesSQL); err != nil {
		return err
	}
	if _, err := Pool.Exec(context.Background(),
		"CREATE INDEX IF NOT EXISTS idx_chat_files_room ON chat_files (room_id)"); err != nil {
		return err
	}
	if _, err := Pool.Exec(context.Background(),
		"CREATE INDEX IF NOT EXISTS idx_chat_files_uploader ON chat_files (uploader_id)"); err != nil {
		return err
	}

	if SeedDemoAccounts {
		log.Println("✅ 数据表与种子数据初始化完成（调试模式：教师 admin01~admin10 / 学生 u001~u100，初始密码随机生成）")
	} else {
		log.Println("✅ 数据表与种子数据初始化完成（未预置调试账户）")
	}
	return nil
}

// randomPassword 生成 8 位随机密码，剔除易混淆字符（0/O/1/l/I）。
func randomPassword() string {
	const chars = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			log.Fatalf("🛑 生成随机密码失败: %v", err)
		}
		b[i] = chars[n.Int64()]
	}
	return string(b)
}

// writeInitialAccounts 把本次新建账号的初始密码写入 data/initial_accounts.txt，
// 供管理员分发账号；仅在确有新账号时写入。文件包含明文密码，提醒用户妥善保管。
func writeInitialAccounts(created map[string]string) error {
	if len(created) == 0 {
		return nil
	}
	if err := os.MkdirAll("./data", 0o755); err != nil {
		return err
	}
	path := filepath.Join("./data", "initial_accounts.txt")
	if _, err := os.Stat(path); err == nil {
		// 已有清单则追加，避免覆盖教师手工维护的内容
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		return writeAccountLines(f, created)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("本文件由服务首次初始化生成，内含初始密码，请分发后妥善保管或删除。\n"); err != nil {
		return err
	}
	return writeAccountLines(f, created)
}

func writeAccountLines(f *os.File, created map[string]string) error {
	names := make([]string, 0, len(created))
	for name := range created {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := fmt.Fprintf(f, "%s\t%s\n", name, created[name]); err != nil {
			return err
		}
	}
	log.Printf("🔑 已生成 %d 个新账号，初始密码清单见 data/initial_accounts.txt（请提醒用户首次登录后修改密码）", len(created))
	return nil
}

func padZero(num int, width int) string {
	s := string(rune('0'+num/100%10)) + string(rune('0'+num/10%10)) + string(rune('0'+num%10))
	return s[len(s)-width:]
}

// ensureColumn 对旧库补列（新库建表已含全部列，此助手仅作前瞻性保留）。
func ensureColumn(table, columnDef string) error {
	_, err := Pool.Exec(context.Background(), "ALTER TABLE "+table+" ADD COLUMN "+columnDef)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return nil
	}
	return err
}
