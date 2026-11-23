# 个人网站后端

## 功能简介

本站的 Go 服务提供 REST API，并通过维护 CLI 管理数据库和站主账号。P1 已实现公开站点配置读取，以及站主登录、会话查询和退出，已通过本地 HTTPS 和浏览器联调。内容发布、Git 同步和访客评论仍在开发计划中。

## 技术栈

Go 1.27.4 使用标准库 `net/http` 处理请求，通过 `database/sql` 和 modernc SQLite 驱动访问数据库，使用 Goose 执行 SQL 迁移，使用 Argon2id 校验密码。依赖版本记录在 [go.mod](go.mod) 和 [go.sum](go.sum) 中。

## 启动、验证与构建

**完整工作区联调。** 后端是[个人网站根仓库](https://github.com/calm195/my-web)的 `backend/` 子模块。首次获取完整源码可执行 `git clone --recurse-submodules git@github.com:calm195/my-web.git`。在根目录 `my-web/` 运行以下命令。

```sh
./dev install
./dev up
./dev status
curl --cacert var/local/certs/ca.pem https://localhost:8443/api/v1/public/site
./dev down
```

默认地址下，该请求应返回公开站点配置 JSON。根工具准备本地证书、SQLite、站主账号并启动服务。管理端入口为 `https://localhost:8443/admin/`。接口与浏览器联合验收可从根目录运行 `./dev acceptance`。

**单独检出后端。** 在后端仓库根目录运行以下命令。将 Go 1.27.4 加入 `PATH`，首次拉取 Go 模块需要网络，运行 race 测试需要本机 C 工具链。

```sh
sh scripts/check.sh
sh scripts/test.sh
sh scripts/build.sh
```

`check.sh` 检查 gofmt 与 go vet，`test.sh` 使用临时数据库运行 `go test -race ./...`，`build.sh` 以 `CGO_ENABLED=0` 生成 `bin/api` 与 `bin/site-admin`。三个入口均检查 Go 版本并设置 `GOTOOLCHAIN=local`。在完整工作区也可以使用根工具链，例如从根目录执行 `./dev exec sh backend/scripts/check.sh`、`./dev exec sh backend/scripts/test.sh` 或 `./dev build api`。

**部署与维护。** 当前验证范围为本地环境，生产部署待实施。`bin/api` 启动时检查 schema 版本。迁移须先通过 `bin/site-admin migrate` 显式执行。维护 CLI 由 `SITE_DATABASE_PATH` 和 `SITE_ASSET_DIR` 指定绝对路径，支持 `diagnose`、`init-admin --username NAME`、`reset-password --username NAME`。初始化和改密都从标准输入读取密码，改密会撤销该站主的全部会话。环境变量见[配置样例](../.env.example)。

## 功能模块与目录

| 要修改或排查的功能 | 代码入口 |
| --- | --- |
| API 启动、路由与错误响应 | [服务进程](cmd/api/main.go)、[HTTP 处理器](internal/httpapi/server.go) |
| 站主认证、会话查询和登录限频 | [认证服务](internal/auth/service.go)、[密码校验](internal/auth/password.go)、[限频器](internal/auth/limiter.go) |
| 公开站点配置 | [公开字段查询](internal/settings/service.go) |
| 存储、迁移和运行配置 | [数据库连接与 schema 检查](internal/store/store.go)、[SQL 迁移](migrations/)、[环境配置校验](internal/config/config.go) |
| 维护 CLI | [迁移、初始化、改密与诊断入口](cmd/site-admin/main.go) |

## 文档导航

单独克隆后端时，可从[根仓库主页](https://github.com/calm195/my-web)取得合同与跨端文档。核对版本时，在候选根提交的工作区执行 `git ls-tree HEAD backend`，将其中的提交哈希与独立后端仓库的 `git rev-parse HEAD` 比对。两者相同即可使用该根提交的合同。已有完整工作区在切换根版本后执行 `git submodule update --init --recursive`，检出该版本固定的前后端代码。

| 阅读任务 | 入口 |
| --- | --- |
| 修改登录、查询会话或退出流程 | [站主登录与会话开发须知](../docs/development/owner-session.md) |
| 核对接口、事务与数据库约束 | [OpenAPI](../contracts/openapi.yaml)、[数据库设计](../docs/design/database-design.md) |
| 配置本地 HTTPS 或复现验收 | [本地开发说明](../docs/development/p1-local.md)、[联调规范](../docs/local-integration.md)、[验收记录](../docs/verification/tools-2026-11-23.md) |
| 查看代码与协作要求 | [后端规范](AGENTS.md)、[根编码规范](../CONTRIBUTING.md) |
