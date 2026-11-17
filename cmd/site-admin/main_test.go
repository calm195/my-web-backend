package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIExplicitPrepareInitializeAndReset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SITE_DATABASE_PATH", filepath.Join(dir, "db", "site.db"))
	t.Setenv("SITE_ASSET_DIR", filepath.Join(dir, "assets"))
	t.Setenv("SITE_PUBLIC_ORIGIN", "https://localhost:8443")
	t.Setenv("SITE_ADDR", "127.0.0.1:8080")
	var out bytes.Buffer
	if err := run([]string{"diagnose"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("诊断不能隐式创建数据库")
	}
	if err := run([]string{"migrate"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"diagnose"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d["admin_initialized"] != false || d["foreign_keys"] != float64(1) || d["journal_mode"] != "wal" {
		t.Fatal("诊断值不正确", d)
	}
	out.Reset()
	if err := run([]string{"init-admin", "--username", "owner"}, strings.NewReader("test-password-only-1234\n"), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "password") || strings.Contains(out.String(), "owner") {
		t.Fatal("运维成功输出不应包含凭据")
	}
	if err := run([]string{"init-admin", "--username", "owner"}, strings.NewReader("another-password-1234\n"), &out); err == nil {
		t.Fatal("重复初始化不能覆盖账号")
	}
	if err := run([]string{"reset-password", "--username", "owner"}, strings.NewReader("replacement-password-1234\n"), &out); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"reset-password", "--username", "owner", "--password", "not-allowed"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("密码不能从参数读取")
	}
	out.Reset()
	if err := run([]string{"diagnose"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &d); err != nil || d["admin_initialized"] != true {
		t.Fatal("初始化状态未更新")
	}
}
