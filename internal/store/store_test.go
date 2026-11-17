package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "site.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMigrationAndReadOnlyStartup(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "site.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CheckSchema(ctx); err == nil {
		t.Fatal("未迁移数据库不能通过启动检查")
	}
	var tables int
	// 版本检查只能读，不允许隐式生成 Goose 元数据。
	if err = s.Read.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='goose_db_version'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("检查意外创建迁移表：%d %v", tables, err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	// 重复迁移不能覆盖站主后来设置的标题。
	if _, err = s.Write.ExecContext(ctx, "UPDATE site_settings SET title='保留标题' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var title string
	if err = s.Read.QueryRowContext(ctx, "SELECT title FROM site_settings WHERE id=1").Scan(&title); err != nil || title != "保留标题" {
		t.Fatalf("重复迁移丢失设置: %s %v", title, err)
	}
	if _, err = s.Read.ExecContext(ctx, "UPDATE site_settings SET title='禁止写入' WHERE id=1"); err == nil {
		t.Fatal("只读连接不应接受写入")
	}
}

func TestEveryConnectionEnablesForeignKeys(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	connections := []*sql.Conn{}
	for i := 0; i < 4; i++ {
		c, err := s.Read.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, c)
	}
	defer func() {
		for _, c := range connections {
			c.Close()
		}
	}()
	for _, c := range connections {
		var fk, busy int
		var mode string
		// 同时占有四个物理连接，验证不是只对首个连接开启外键。
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
			t.Fatalf("fk=%d %v", fk, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil || busy != 5000 {
			t.Fatalf("busy=%d %v", busy, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
			t.Fatalf("mode=%s %v", mode, err)
		}
	}
}

func TestImmediateWriteLockAndRollback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "site.db")
	s, err := Open(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	c, err := other.Write.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// 测试专用连接将锁等待缩至 50 ms，不修改应用默认 5000 ms。
	if _, err = c.ExecContext(ctx, "PRAGMA busy_timeout=50"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.BeginTx(ctx, nil)
	if !IsBusy(err) {
		tx.Rollback()
		t.Fatalf("BEGIN 前尚未写数据也必须占写锁: %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	marker := errors.New("故障注入")
	err = s.WithTx(ctx, func(tx *sql.Tx) error {
		// 事务执行部分修改后失败，原始标题必须完整保留。
		if _, e := tx.ExecContext(ctx, "UPDATE site_settings SET title='不可提交' WHERE id=1"); e != nil {
			return e
		}
		return marker
	})
	if !errors.Is(err, marker) {
		t.Fatal(err)
	}
	var title string
	if err = s.Read.QueryRowContext(ctx, "SELECT title FROM site_settings WHERE id=1").Scan(&title); err != nil || title == "不可提交" {
		t.Fatalf("事务未回滚 %s %v", title, err)
	}
}

func TestDeferredForeignKeyRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		// 复合 FK 延迟到 COMMIT：引用不存在版本时，整对象必须一起回滚。
		_, e := tx.ExecContext(ctx, `INSERT INTO content_objects(id,kind,draft_revision_id,created_at,updated_at) VALUES('object','article','missing',1,1)`)
		return e
	})
	if err == nil {
		t.Fatal("延迟外键必须拒绝提交")
	}
	var n int
	if err = s.Read.QueryRowContext(ctx, "SELECT count(*) FROM content_objects").Scan(&n); err != nil || n != 0 {
		t.Fatalf("残留内容对象 %d %v", n, err)
	}
}

func TestSchemaCheckHonorsLatestMigrationRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// 模拟维护后留存的回退元数据，不执行破坏性 Down；旧的 version=1 应用记录不能误放行。
	if _, err := s.Write.ExecContext(ctx, "INSERT INTO goose_db_version(version_id,is_applied) VALUES(1,0)"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckSchema(ctx); err == nil {
		t.Fatal("已回退的 schema 不能通过当前二进制检查")
	}
}
