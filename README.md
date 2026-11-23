# 个人网站后端

Go REST API 与维护 CLI 的独立仓库，以 Git 子模块纳入个人网站根仓库。当前 P1 实现数据库底座、站主会话认证和公开站点配置；内容发布与 Git 同步等按后续阶段开发。

完整设计与合同统一位于根仓库：[开发规范](../AGENTS.md)、[OpenAPI](../contracts/openapi.yaml)、[数据库设计](../docs/design/database-design.md)、[本地 HTTPS 说明](../docs/development/p1-local.md)。本仓库不保存这些文件的副本。单独克隆时这些链接需在完整根工作区中查看。

## 独立检查、测试和构建

需要 Go **1.27.4**，命令通过 `PATH` 选择工具。三个入口设置 `GOTOOLCHAIN=local` 并拒绝其他 Go 版本，不会自动下载另一版工具链。首次解析 Go 依赖需要网络；`go.mod` 与 `go.sum` 固定依赖，无 Node 或前端目录依赖。

在本仓库根目录执行：

```sh
sh scripts/check.sh
sh scripts/test.sh
sh scripts/build.sh
```

`check` 验证 gofmt 与 go vet，格式不符合时只报告；`test` 执行 `go test -race ./...`，需要平台 C 工具链以支持 race，数据库用例使用独立临时目录；`build` 使用 `CGO_ENABLED=0` 生成 `bin/api` 与 `bin/site-admin`，不嵌入前端产物。产物与运行数据已忽略。

在完整根工作区可使用项目私有工具链，从根目录执行：

```sh
./dev exec sh backend/scripts/check.sh
./dev exec sh backend/scripts/test.sh
./dev exec sh backend/scripts/build.sh
```

## 本地运行与维护

本地 HTTPS 的证书、密钥、SQLite、账号、启动与停止由根仓库脚本统一管理；按[本地联调文档](../docs/local-integration.md)执行。后端启动仅检查数据库 schema，迁移由维护 CLI 显式执行，不在进程启动时自动改表。

维护 CLI 支持 `migrate`、`init-admin --username NAME`、`reset-password --username NAME`、`diagnose`。密码从标准输入读取，不放进参数；改密清除该站主全部会话。运行配置与安全边界见根仓库 `.env.example` 和本地开发说明，不提交真实配置、证书、凭据或数据库。

后端独立构建或测试通过不代表合同、真实 HTTPS 或浏览器联调通过；完整验收记录统一保留在根仓库 `docs/verification/`。发布先确保子仓库提交可获取，再由根仓库更新子模块指针确定被验收的版本组合。
