// Package store 管理 SQLite 读写连接和事务；业务服务决定事务边界。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	"modernc.org/sqlite"
	"personal-website/backend/migrations"
)

// SchemaVersion 是当前二进制唯一支持的数据库版本，未来迁移必须同步提高。
const SchemaVersion int64 = 1

// Store 的单写连接避免进程内写入竞争；读池只读以阻止查询代码意外修改数据库。
type Store struct{ Write, Read *sql.DB }

// Open 只打开连接，不创建业务表。create 仅供显式迁移使用。
func Open(ctx context.Context, path string, create bool) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("数据库路径必须是绝对路径")
	}
	if create {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	} else if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("数据库尚未准备，请先运行 migrate: %w", err)
	}
	mode := "rw"
	if create {
		mode = "rwc"
	}
	w, err := sql.Open("sqlite", dsn(path, mode, true))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	if err = w.PingContext(ctx); err != nil {
		w.Close()
		return nil, err
	}
	// WAL 在事务外配置；后续每个读写连接仍通过 DSN 独立启用 FK 和 busy_timeout。
	if _, err = w.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, "ro", false))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	r.SetMaxIdleConns(4)
	if err = r.PingContext(ctx); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	if create {
		if err = os.Chmod(path, 0600); err != nil {
			w.Close()
			r.Close()
			return nil, err
		}
	}
	return &Store{Write: w, Read: r}, nil
}

func dsn(path, mode string, write bool) string {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {mode}, "_pragma": {"foreign_keys(1)", "busy_timeout(5000)"}}
	// modernc 的 _txlock=immediate 令 database/sql BeginTx 实际取得保留写锁。
	if write {
		q.Set("_txlock", "immediate")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Close 关闭本 Store 拥有的两个连接池。
func (s *Store) Close() error { return errors.Join(s.Read.Close(), s.Write.Close()) }

// WithTx 在单写连接中执行并提交事务，任一错误或延迟外键失败都回滚全部修改。
func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// 回调内的全部数据库操作必须使用 tx；混用连接池会逃离事务并可能等待自身持有的写连接。
	// 回调失败时延迟回滚；提交后 database/sql 将事务标记为完成，延迟回滚不再改变数据库。
	// 当前 modernc 驱动在提交失败时尝试清理仍活动的事务；升级驱动时需重新验证这一边界。
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Migrate 仅供运维 CLI 调用；Goose 同事务记录版本，重复执行不重建表或重置站点设置。
func (s *Store) Migrate(ctx context.Context) error {
	p, err := goose.NewProvider(goose.DialectSQLite3, s.Write, migrations.Files)
	if err != nil {
		return err
	}
	if _, err = p.Up(ctx); err != nil {
		return err
	}
	// 单例初始设置只在首次准备时写入；后续迁移保留站主已有名称和描述。
	_, err = s.Write.ExecContext(ctx, `INSERT OR IGNORE INTO site_settings(id,title,description,timezone,generation,updated_at) VALUES(1,? ,?,'Asia/Shanghai',1,?)`, "一隅", "记录技术，也记录生活。", time.Now().UTC().UnixMilli())
	return err
}

// CheckSchema 只读 Goose 版本，不创建迁移元数据，不自动升级运行中的数据库。
func (s *Store) CheckSchema(ctx context.Context) error {
	var version int64
	// 同一版本只取最后一条记录，与 Goose 回退记录语义一致；不能误把已回退版本的旧应用记录当成当前版本。
	if err := s.Read.QueryRowContext(ctx, `SELECT g.version_id FROM goose_db_version g WHERE g.is_applied=1 AND NOT EXISTS (SELECT 1 FROM goose_db_version newer WHERE newer.version_id=g.version_id AND newer.id>g.id) ORDER BY g.id DESC LIMIT 1`).Scan(&version); err != nil {
		return fmt.Errorf("缺少 schema，请先运行 migrate: %w", err)
	}
	if version != SchemaVersion {
		return fmt.Errorf("schema 版本不匹配：需要 %d，实际 %d", SchemaVersion, version)
	}
	return nil
}

// IsBusy 将实际驱动的锁冲突映射到稳定 API 业务错误，不向访客泄漏 SQL。
func IsBusy(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && (e.Code()&255 == 5 || e.Code()&255 == 6)
}

// Diagnostics 仅供私有 CLI/探针使用，不经公开站点接口返回数据库实现信息。
func (s *Store) Diagnostics(ctx context.Context) (map[string]any, error) {
	var v, mode string
	var fk, busy int
	// 诊断读取当前连接的引擎版本、外键及锁等待设置，单位为毫秒。
	if err := s.Read.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&v); err != nil {
		return nil, err
	}
	if err := s.Read.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		return nil, err
	}
	if err := s.Read.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return nil, err
	}
	if err := s.Read.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
		return nil, err
	}
	var count int
	// 只返回账号是否已初始化，不返回账号名、密码摘要或会话数量。
	if err := s.Read.QueryRowContext(ctx, "SELECT count(*) FROM admin_users").Scan(&count); err != nil {
		return nil, err
	}
	return map[string]any{"sqlite_version": v, "foreign_keys": fk, "journal_mode": mode, "busy_timeout_ms": busy, "schema_version": SchemaVersion, "admin_initialized": count == 1}, nil
}
