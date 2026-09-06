// Copyright (c) 黄智韬. All rights reserved.

package handlers

import (
	"context"
	"path/filepath"
	"testing"

	"visualclassroom/classserver/config"
	"visualclassroom/classserver/database"
)

// TestStudentStatsSQL 冒烟测试：验证学生统计 SQL（FILTER/CAST 语法）在 SQLite 上可用。
func TestStudentStatsSQL(t *testing.T) {
	cfg := &config.Config{DBPath: filepath.Join(t.TempDir(), "test.db"), DBMaxConns: 2}
	if err := database.InitDB(cfg); err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	defer database.CloseDB() // 释放文件句柄，否则 t.TempDir 清理会失败
	ctx := context.Background()
	// 最小化建表
	for _, ddl := range []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, role TEXT, full_name TEXT)",
		`CREATE TABLE essay_submissions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER, username TEXT, title TEXT, content TEXT, type TEXT,
			status TEXT DEFAULT 'done', feedback TEXT DEFAULT '', score_text TEXT,
			max_score INTEGER DEFAULT 100, student_name TEXT,
			is_streaming BOOLEAN DEFAULT FALSE, teacher_edited BOOLEAN DEFAULT FALSE,
			is_deleted BOOLEAN DEFAULT FALSE, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			image_path TEXT, teacher_comment TEXT, annotated_path TEXT, annotations TEXT,
			excellent BOOLEAN DEFAULT FALSE, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)`,
		"INSERT INTO users (username, role, full_name) VALUES ('stu1', 'student', '张三')",
		"INSERT INTO essay_submissions (user_id, username, content, type, score_text, max_score) VALUES (1, 'stu1', 'content', 'chinese', '48', '60')",
	} {
		if _, err := database.Pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("建表失败: %v", err)
		}
	}
	rows, err := database.Pool.Query(ctx,
		`SELECT u.id, u.username, COALESCE(u.full_name,''),
		        COALESCE(COUNT(s.id) FILTER (WHERE s.is_deleted=false), 0),
		        COALESCE(AVG(CAST(s.score_text AS REAL) / s.max_score * 100) FILTER (
		            WHERE s.is_deleted=false AND s.status='done' AND s.score_text IS NOT NULL
		                  AND s.score_text <> '' AND s.score_text <> '--' AND s.score_text <> '错误'), NULL),
		        COALESCE(COUNT(s.id) FILTER (WHERE s.is_deleted=false AND s.excellent), 0),
		        COALESCE(MAX(s.created_at) FILTER (WHERE s.is_deleted=false), '')
		 FROM users u LEFT JOIN essay_submissions s ON s.user_id = u.id
		 WHERE u.role='student'
		 GROUP BY u.id, u.username, u.full_name
		 ORDER BY u.id`)
	if err != nil {
		t.Fatalf("学生统计 SQL 失败: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, essayCount, excellentCount int
		var username, fullName string
		var avg *float64
		var last string
		if err := rows.Scan(&id, &username, &fullName, &essayCount, &avg, &excellentCount, &last); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		if avg == nil {
			t.Fatalf("应计算出均分")
		}
		if *avg < 79.9 || *avg > 80.1 {
			t.Fatalf("均分应为 80（48/60），实际 %v", *avg)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("应返回 1 个学生，实际 %d", count)
	}
}
