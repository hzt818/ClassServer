package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"visualclassroom/classserver/config"
)

// Pool 与原 pgxpool.Pool 的使用面保持同签名（Exec/Query/QueryRow/Begin），
// 底层换成嵌入式 SQLite（modernc.org/sqlite，纯 Go、无 CGO），
// 教师机器上双击即可运行，不再需要安装/配置 PostgreSQL 服务。
//
// SQL 兼容处理（见 adaptSQLArgs）：
//   - $n 占位符自动改写为 SQLite 的 ? 占位符；
//   - "= ANY($n)" 且第 n 个参数是切片时自动展开为 IN (?, ?, ...)，
//     因此处理器里所有 ANY 写法无需逐处修改。
var Pool *DB

// ErrNoRows 与 database/sql 一致，供调用方判断"查询无结果"。
var ErrNoRows = sql.ErrNoRows

type DB struct {
	db *sql.DB
}

// CommandTag 模拟 pgconn.CommandTag，仅暴露 RowsAffected。
type CommandTag struct {
	rowsAffected int64
}

func (t CommandTag) RowsAffected() int64 { return t.rowsAffected }

// Rows 模拟 pgx.Rows 的使用面（Next 无参 / Scan / Close / Err）。
type Rows struct {
	rs  *sql.Rows
	err error
}

func (r *Rows) Next() bool {
	if r == nil || r.rs == nil {
		return false
	}
	return r.rs.Next()
}

func (r *Rows) Scan(dest ...any) error {
	if r == nil || r.rs == nil {
		return r.err
	}
	return r.rs.Scan(dest...)
}

func (r *Rows) Close() error {
	if r == nil || r.rs == nil {
		return nil
	}
	return r.rs.Close()
}

func (r *Rows) Err() error {
	if r == nil || r.rs == nil {
		return r.err
	}
	return r.rs.Err()
}

// Tx 模拟 pgx.Tx 的使用面（方法都带 ctx 参数）。
type Tx struct {
	tx *sql.Tx
}

func (t *Tx) Exec(ctx context.Context, query string, args ...any) (CommandTag, error) {
	q, flat := adaptSQLArgs(query, args)
	res, err := t.tx.ExecContext(ctx, q, flat...)
	if err != nil {
		return CommandTag{}, err
	}
	n, _ := res.RowsAffected()
	return CommandTag{rowsAffected: n}, nil
}

func (t *Tx) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	q, flat := adaptSQLArgs(query, args)
	rs, err := t.tx.QueryContext(ctx, q, flat...)
	if err != nil {
		return &Rows{err: err}, err
	}
	return &Rows{rs: rs}, nil
}

func (t *Tx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	q, flat := adaptSQLArgs(query, args)
	return t.tx.QueryRowContext(ctx, q, flat...)
}

func (t *Tx) Commit(ctx context.Context) error { return t.tx.Commit() }

func (t *Tx) Rollback(ctx context.Context) error { return t.tx.Rollback() }

// ---------- SQL 方言适配 ----------

var paramRe = regexp.MustCompile(`\$(\d+)`)

// adaptSQLArgs 把 pgx 风格的 SQL + 参数改写为 SQLite 可执行的形式。
//   - "$n" 占位符 → "?"（SQLite 按出现顺序编号，与无复用的 $n 一一对应）；
//   - "$n" 对应的参数是切片（且非 []byte）时，视为 PG 数组：把 "= ANY($n)" 整体展开为
//     "IN (?, ?, ...)"（SQLite 没有 ANY 函数，必须连关键字一起替换），
//     切片元素按占位顺序摊平进参数表（空集合展开为 "(NULL)"，恒不命中）。
func adaptSQLArgs(query string, args []any) (string, []any) {
	expandable := make(map[int][]any) // 参数序号(1-based) -> 切片元素
	for i, arg := range args {
		if isExpandableSlice(arg) {
			expandable[i+1] = sliceElements(arg)
		}
	}

	flat := make([]any, 0, len(args)+8)
	var out strings.Builder
	last := 0
	for _, loc := range paramRe.FindAllStringSubmatchIndex(query, -1) {
		prefix := query[last:loc[0]]
		last = loc[1]
		n := 0
		for _, c := range query[loc[2]:loc[3]] {
			n = n*10 + int(c-'0')
		}
		if n < 1 || n > len(args) {
			// 参数越界：保留原样，让底层报出可定位的错误
			out.WriteString(prefix)
			out.WriteString(query[loc[0]:loc[1]])
			continue
		}
		if elems, ok := expandable[n]; ok {
			// PG 数组展开：把前导的 "= ANY("（或裸 "ANY("）替换为 "IN ("，
			// 占位符本身替换为参数列表；SQLite 没有 ANY 函数，必须连关键字一起替换，
			// 并吞掉原 "ANY($n)" 里 $n 之后的右括号。
			p := prefix
			trimmed := strings.TrimRight(p, " \t")
			switch {
			case strings.HasSuffix(trimmed, "= ANY("):
				p = p[:len(p)-len(trimmed)] + trimmed[:len(trimmed)-len("= ANY(")]
			case strings.HasSuffix(trimmed, "ANY("):
				p = p[:len(p)-len(trimmed)] + trimmed[:len(trimmed)-len("ANY(")]
			}
			out.WriteString(p)
			if len(elems) == 0 {
				out.WriteString("IN (NULL)")
			} else {
				out.WriteString("IN (")
				for i := range elems {
					if i > 0 {
						out.WriteString(",")
					}
					out.WriteString("?")
					flat = append(flat, elems[i])
				}
				out.WriteString(")")
			}
			// 吞掉 "ANY($n)" 的收尾右括号（SQLite 中它已无归属）
			if strings.HasPrefix(query[last:], ")") {
				last++
			}
			continue
		}
		out.WriteString(prefix)
		out.WriteString("?")
		flat = append(flat, args[n-1])
	}
	out.WriteString(query[last:])
	return out.String(), flat
}

func isExpandableSlice(v any) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		// []byte 是二进制大对象，不能展开成 IN 列表
		return rv.Type().Elem().Kind() != reflect.Uint8
	default:
		return false
	}
}

func sliceElements(v any) []any {
	rv := reflect.ValueOf(v)
	out := make([]any, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out[i] = rv.Index(i).Interface()
	}
	return out
}

// ---------- 初始化 ----------

// InitDB 打开（必要时创建）SQLite 数据库并配置 WAL 并发参数。
func InitDB(cfg *config.Config) error {
	dbPath := cfg.DBPath
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建数据目录失败: %w", err)
		}
	}

	dsn := "file:" + dbPath +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("打开 SQLite 数据库失败: %w", err)
	}

	maxConns := cfg.DBMaxConns
	if maxConns < 1 {
		maxConns = 1
	}
	sqlDB.SetMaxOpenConns(maxConns)
	sqlDB.SetMaxIdleConns(maxConns)
	sqlDB.SetConnMaxIdleTime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return fmt.Errorf("SQLite 数据库不可用: %w", err)
	}

	Pool = &DB{db: sqlDB}
	log.Println("✅ SQLite 数据库初始化成功（WAL 模式）:", dbPath)
	return nil
}

func (d *DB) Exec(ctx context.Context, query string, args ...any) (CommandTag, error) {
	q, flat := adaptSQLArgs(query, args)
	res, err := d.db.ExecContext(ctx, q, flat...)
	if err != nil {
		return CommandTag{}, err
	}
	n, _ := res.RowsAffected()
	return CommandTag{rowsAffected: n}, nil
}

func (d *DB) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	q, flat := adaptSQLArgs(query, args)
	rs, err := d.db.QueryContext(ctx, q, flat...)
	if err != nil {
		return &Rows{err: err}, err
	}
	return &Rows{rs: rs}, nil
}

func (d *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	q, flat := adaptSQLArgs(query, args)
	return d.db.QueryRowContext(ctx, q, flat...)
}

func (d *DB) Begin(ctx context.Context) (*Tx, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

// Stats 返回底层连接池统计（供开发者控制台 occupy 命令使用，
// 对应原 pgxpool Stat() 的使用面：InUse≈AcquiredConns、Idle≈IdleConns）。
func (d *DB) Stats() sql.DBStats { return d.db.Stats() }

// GetPool 兼容原 pgx 版本的导出函数（返回适配器实例）。
func GetPool() *DB { return Pool }

func CloseDB() {
	if Pool != nil {
		Pool.db.Close()
		log.Println("✅ 数据库已关闭")
	}
}
