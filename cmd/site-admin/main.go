// Command site-admin 负责私有运维操作；密码从标准输入读取，不通过命令参数或日志传递。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"personal-website/backend/internal/auth"
	"personal-website/backend/internal/config"
	"personal-website/backend/internal/store"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "运维操作失败：", err)
		os.Exit(1)
	}
}
func run(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("用法：site-admin migrate|init-admin --username NAME|reset-password --username NAME|diagnose")
	}
	command := args[0]
	if command != "migrate" && command != "init-admin" && command != "reset-password" && command != "diagnose" {
		return errors.New("未知命令")
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	username := fs.String("username", "", "站主账号名")
	if err := fs.Parse(args[1:]); err != nil {
		return errors.New("参数不正确；密码必须通过标准输入")
	}
	if fs.NArg() != 0 {
		return errors.New("不接受多余参数")
	}
	c, err := config.Load(false)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if command == "migrate" {
		if err = os.MkdirAll(c.AssetDir, 0700); err != nil {
			return err
		}
	}
	db, err := store.Open(ctx, c.DatabasePath, command == "migrate")
	if err != nil {
		return err
	}
	defer db.Close()
	if command == "migrate" {
		if err = db.Migrate(ctx); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"status": "ok", "schema_version": store.SchemaVersion})
	}
	if err = db.CheckSchema(ctx); err != nil {
		return err
	}
	if command == "diagnose" {
		d, err := db.Diagnostics(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(d)
	}
	if strings.TrimSpace(*username) == "" {
		return errors.New("必须指定 --username")
	}
	// 限制 stdin 大小，去掉行终止符而不 trim 密码正文；从参数读取密码会暴露在进程列表。
	r := bufio.NewReader(io.LimitReader(in, 2049))
	password, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return errors.New("无法读取密码")
	}
	if len(password) > 2048 {
		return errors.New("密码输入过长")
	}
	password = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
	if command == "init-admin" {
		err = auth.InitAdmin(ctx, db, *username, password)
	} else {
		err = auth.ResetPassword(ctx, db, *username, password)
	}
	if err != nil {
		return errors.New("账号操作未完成，请检查账号是否存在及密码是否为 12–256 字符；现有数据不会被覆盖")
	}
	return json.NewEncoder(out).Encode(map[string]string{"status": "ok"})
}
