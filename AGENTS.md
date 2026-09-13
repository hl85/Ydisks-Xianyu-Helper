# AGENTS.md

> 本文件只写规则：一条规则一句话，每句不超过 50 字。
> 规则按开发阶段分组，组内按性质分小节；命令只放代码块。

## 目录

| 阶段 | 内容 |
|---|---|
| 0 开工前 | 环境、编译、必读文档、阶段制度、门禁、任务工作流 |
| 1 编码中 | 架构、注释、API、前端、数据库、并发、敏感数据、冻结项 |
| 2 验证 | 测试与覆盖率、门禁命令、禁止事项 |
| 3 提交与评审 | 提交颗粒度、适配器、打 tag |
| 4 构建与发布 | 镜像、桌面打包、macOS、CI |
| 5 仓库与文档 | 目录职责、运行时接线 |

---

## 0. 开工前

### 0.1 环境

- 本机没有 Go 工具链，编译与测试一律在容器内执行。
- docker CLI 不在 PATH，用 Docker.app 里的全路径调用。
- 容器镜像 golang:1.26，内含 go1.26.8，满足 go.mod 要求。
- 本机是 Intel Mac，容器是 linux/amd64，产物不能直接在本机运行。
- 本机磁盘紧张，构建大镜像前先看 df -h 剩余空间。

```bash
DOCKER=/Applications/Docker.app/Contents/Resources/bin/docker
REPO=/Users/wanghui/Code/oss-research/Ydisks-Xianyu-Helper
COMPOSE="$DOCKER compose -f docker-compose.functional.yml"
cd "$REPO"
```

### 0.2 编译

```bash
"$DOCKER" run --rm -v "$REPO":/src -w /src -v ydisks-gomod:/go/pkg/mod \
  golang:1.26 go build -o /tmp/xianyu-server ./cmd/server
```

- 依赖缓存在 ydisks-gomod 卷，首次约 1 分 40 秒。
- 后续复用该卷，不再重复下载依赖。
- 多入口在同一个容器内串联编译。
- 禁止为每个入口各起一个容器。
- cmd/tray 依赖桌面图形库，禁止在容器内构建。
- 产物禁止写进源码树，统一输出到 /tmp 或 dist。
- 交叉编译 macOS 产物要指定 GOOS=darwin 与 GOARCH=amd64。
- 编译报 undefined 时先查并发编辑，别把半成品当缺陷。

### 0.3 必读文档

- 改包边界、API、数据库、凭证、接线、前端或 CI 前，必须读总计划。
- 必须同时读架构依赖规则与注释标准两份文档。
- 涉及凭证、登录、引擎、账号或服务端链路时，追加读冻结规范。
- 当前阶段只由重构总计划定义，动手前先读它的状态表。
- 禁止从旧提交、文档标题或历史声明推断阶段。

| 文档 | 路径 |
|---|---|
| 重构总计划 | docs/architecture/refactoring-master-plan.md |
| 架构依赖规则 | docs/architecture/dependency-rules.md |
| 注释标准 | docs/architecture/comment-standard.md |
| 验证码冻结规范 | docs/slider-captcha-frozen-spec.md |

### 0.4 阶段制度

- 权威排期只有六个阶段，只由重构总计划定义。
- 一个阶段就是一个任务、一次评审、一条中文提交。
- 禁止把阶段拆成切片、里程碑、独立 PR 或中间提交。
- 阶段进行中可临时编译不过，最终提交必须编译通过。
- 最终提交必须能启动并通过全部列出的验证。
- 验证证据只在重构进度文档记录一次。

### 0.5 门禁

- 迁移开始前就要建立完整的架构门禁目录。
- 架构检查工具读当前阶段并启用该阶段及之前所有门禁。
- 阶段状态缺失或含糊时，架构检查必须失败。
- 第六阶段完成后，所有门禁永久生效。
- 禁止把门禁设计推迟到第六阶段。
- 禁止提前启用后续阶段的门禁。
- 禁止用白名单、基线或忽略路径绕过门禁。
- 禁止把门禁降级为仅告警。

### 0.6 任务工作流（worktree）

- 本节管本 clone 的改进性任务。
- 六阶段重构仍走阶段制度那一节。
- 任务开始前必须新建 worktree。
- 禁止在主工作区直接开发。
- 主工作区保持为部署工作区，用于构建镜像与 compose。
- worktree 只用于改代码、编译、测试与提交。
- worktree 放在 .worktree 目录下，该目录已被忽略。
- 同一任务期间不要跨 worktree 改同一个文件。
- 并行任务要先确认改动文件清单不重叠。
- 任务合并后用 git worktree remove 清理。

```bash
git worktree add ".worktree/<任务名>" -b "<分支名>"
cd "$REPO/.worktree/<任务名>"
```

---

## 1. 编码中

### 1.1 架构与依赖

- cmd 只做配置、依赖装配、信号与生命周期，不写业务。
- internal/server 只做 HTTP 与 SPA 传输。
- 新用例必须走应用服务，处理器不加业务逻辑。
- 处理器禁止新增 Store.DB、事务、MTOP 或浏览器逻辑。
- 应用服务管用例编排、鉴权与事务边界。
- 应用服务不依赖 net/http、chi 或前端兼容字段。
- 接口由使用方定义并保持最小。
- 禁止服务定位器与万能仓储接口。
- 必需依赖禁止用运行时 setter 注入。
- internal/db 管 SQL、方言、迁移与落库加密。
- internal/db 不反向依赖上层，也不决定 HTTP 响应。
- internal/xianyu 与 internal/browser 管平台与浏览器实现。
- 两者禁止直接写业务数据或决定自动化规则。
- 两者禁止依赖 HTTP 层与应用层。
- internal/engine 与 internal/automation 必须独立于 Server。
- 新增可变并发状态要有归属、锁与关停文档，并配套测试。
- 跨仓库原子操作放在应用级工作单元之后。
- 新处理器禁止直接调用 BeginTx。
- 迁移期旧违规可留到记录阶段完成。
- 禁止新增架构违规。

### 1.2 中文注释

- 新增或修改的函数、方法、匿名函数都要中文语义注释。
- 注释要覆盖参数、返回值、字段与常量。
- 注释要覆盖包级与模块级变量。
- 注释要覆盖局部变量、短声明、循环变量与回调参数。
- 注释要覆盖 React 状态值、setter、ref 与 memo 值。
- 多变量声明适合时用一条就近注释统一说明。
- 注释写业务含义、输入输出语义与单位。
- 注释写生命周期、归属、取消、并发与兼容性约束。
- 注释要写敏感性约束。
- 禁止翻译标识符或复述语法。
- 行为或归属变化时，同一改动内更新注释。
- 重大重构的文件要清除其历史注释基线。
- 生成代码、第三方代码与 webui 静态产物不做补录。
- 禁止占位注释，例如 err 表示错误。
- 注释禁止出现真实 Cookie、Token 或生产密钥。
- 门禁只能证明注释存在并含中文。
- 语义准确性由评审人与 agent 负责。
- 禁止新增注释债务。
- 禁止用基线豁免新建或语义已改的声明。
- 基线重新生成要经评审并记录范围。

```bash
# Go 侧注释门禁（本机无 Go，走容器）
"$DOCKER" run --rm -v "$REPO":/src -w /src -v ydisks-gomod:/go/pkg/mod \
  golang:1.26 go run ./tools/commentlint -mode check -root .

# 前端侧注释门禁（本机有 Node，可本机执行）
npm --prefix frontend run comments:check
```

### 1.3 HTTP API

- 新接口用 /api/v1 前缀，除非本次是兼容性修复。
- 新请求与响应使用具名 DTO。
- 禁止匿名请求结构体与 map 类型响应契约。
- 禁止把数据库模型直接序列化为响应。
- 新失败使用统一错误信封与正确状态码。
- 禁止 HTTP 200 搭配 success 为假。
- 禁止新增 detail、msg 或 error 别名。
- 数据库模型、领域模型与传输 DTO 必须分开。
- 兼容性归一化只能放在传输适配层。
- 路由前缀变更要同步 vite 配置、调用方、契约测试与产物。
- api/openapi.yaml 是版本化接口与健康检查的唯一契约源。
- 新版本化接口要先改规范、重生成类型、补契约测试。
- 生成类型只读，特性 UI 模型留在自己的适配器后。

### 1.4 前端（React）

- 前端依赖方向固定为 app 到 features 到 shared。
- 特性禁止引用另一个特性的内部文件。
- shared 禁止引用 features。
- 组件禁止直接调用 fetch 或 axios。
- 组件通过特性适配器使用共享 HTTP 客户端。
- 领域归一化不要写进通用客户端。
- 禁止把可推导的值放进 state。
- 用户触发的副作用放在事件处理器里。
- 依赖不同的 effect 必须拆分。
- 新值依赖旧值时必须用函数式更新。
- 异步 effect 或请求要有取消与最新代次保护，防过期响应。
- 独立请求应并行发起。
- 没有昂贵计算或稳定子属性理由就不要加 memo。
- 禁止在父组件内定义子组件。
- 服务端数据、表单状态与瞬态 UI 状态必须分开。
- 重页面与重可选依赖按路由或特性懒加载。
- 新用户流程要有成功、失败、取消、切换与过期响应测试。
- 源码字符串测试只留给静态架构规则。
- 生成的 API 类型只读，特性适配器转成 UI 模型。
- 特性代码禁止直接引用生成的 schema。
- 特性代码禁止恢复 transport.ts 或旧的请求函数。
- 只有共享契约运行时可以调用 fetch。

### 1.5 数据库与多方言

- 不得向高层暴露新的裸 *sql.DB。
- 改用窄仓储方法或显式工作单元。
- SQL 行结构、持久化模型、领域模型与 DTO 不得合并。
- 敏感度或归属不同的类型尤其禁止合并。
- 每次迁移保持三种方言的编号与最终 schema 一致。
- 数据库行为变更要有 SQLite 聚焦测试。
- 有环境时补 MySQL 与 Postgres 回归或 dbverify 证据。
- 仓储与包拆分只在消费者接口与事务边界清晰后做。
- 目录数量不是目标。
- 禁止用全局变量或反射隐藏包循环。

### 1.6 并发与生命周期

- 后台协程必须有文档化的归属与 Context 来源。
- 后台协程必须有取消路径与等待路径。
- Start 不得让半构造对象对外可见。
- 契约允许重复关停时 Stop 与 Close 必须幂等。
- 除文档化的归属规则外，由发送方关闭通道。
- 禁止持锁做不受控的网络、浏览器或用户等待 I/O。
- 并发类型要写明哪个锁保护哪些字段。
- 并发类型要写明允许的加锁顺序。
- 必需依赖由构造函数注入并在 Start 前校验。
- 可变 setter 只用于可选运行时配置或隔离测试。
- 可变 setter 不得造成非法的中间生产状态。

### 1.7 敏感数据

- 归属校验禁止读取或解密 Cookie、Token 与密码。
- 禁止读取或解密加密元数据。
- 平台凭证与密码登录密钥用独立模型与专用仓储方法。
- 敏感持久化模型禁止序列化为 HTTP 响应。
- 敏感持久化模型禁止进入前端状态。
- 日志、通知、API 错误与测试输出禁止含明文凭证。
- 新归属校验只返回存在性或非敏感身份。
- 禁止返回解密 Cookie 的映射。
- 非协议强制时禁止跨慢外部 I/O 持凭证锁。
- 持凭证锁时必须文档化并测试锁顺序。

### 1.8 冻结项（滑块验证码）

- 滑块验证码已生产冻结，行为以冻结规范为准。
- 未经本次任务明确授权，禁止改动其实现与测试。
- 禁止重构、优化、重命名、移动、删除或重排受保护代码。
- 禁止改选择器与选择器优先级。
- 禁止改同帧可见性判断。
- 禁止改标准 NC 距离 300px - 42px = 258px。
- 禁止改轨迹、点数、时序与鼠标事件顺序。
- 禁止改主引擎不越界行为。
- 禁止改 x5sec 成功判定。
- 禁止改惩罚与 CAPTCHA 判 URL 逻辑。
- 禁止改重试选择器与重试文案。
- 禁止改来源校验、重载恢复、重试次数与超时。
- 禁止改 Playwright 优先与 CDP 兜底的顺序。
- 禁止改持久化 profile 的复用与锁定。
- 禁止改验证 URL 刷新时机与 Cookie 合并行为。
- 禁止改浏览器参数与环境默认值。
- 禁止改引擎结果标签。
- 禁止弱化、跳过、删除或重写滑块测试。
- 禁止改其他文件以间接改变冻结行为。
- 受保护文件是冻结规范列出的七个文件。
- 只有本次任务明确授权才能改。
- 授权时要同步更新实现、测试、冻结规范并跑完其验证。
- 一次授权不构成后续任务的许可。

---

## 2. 验证

### 2.1 测试与覆盖率

- 新增或修改的确定性函数、分支与错误路径都要有聚焦测试。
- 优先用注入依赖、本地 httptest 与内存数据库。
- 优先用本地 Playwright 页面，避免真实平台调用。
- 覆盖率报告是验证产物，禁止提交。
- cover.out 与 frontend/coverage 保持为生成文件。
- 禁止为提升百分比排除业务文件或降低阈值。
- 禁止把业务代码标记为忽略。
- 禁止为百分比弱化断言。
- 纯 React UI 组件不在业务覆盖率目标内。
- 其业务行为要留在被测的 Hook、状态与服务模块。
- 未覆盖的业务代码要在计划中分类为确定性、仅环境或外部平台。
- 只有真实账号或不可用外部服务才允许跳过测试。
- 本地浏览器行为必须用确定性夹具并保持覆盖。
- 错误处理、取消、生命周期与解析同样要覆盖。
- UI 状态切换也要覆盖。
- 覆盖率声明必须写清执行命令。
- 声明要写明是否启用浏览器集成开关。
- 声明要给出前后端语句覆盖率。
- 声明要列出真实账号与外部平台的例外清单。

```bash
# Go 覆盖率（容器内，默认不启 Chromium）
"$DOCKER" run --rm -v "$REPO":/src -w /src -v ydisks-gomod:/go/pkg/mod \
  golang:1.26 sh -c 'go test -coverprofile=cover.out ./... && go tool cover -func=cover.out | tail -1'

# 前端覆盖率（本机 Node 可执行）
npm --prefix frontend run test:coverage
```

### 2.2 门禁命令

```bash
"$COMPOSE" build go-test go-lint     # 首次构建检查镜像
"$COMPOSE" run --rm go-vet           # go vet ./...
"$COMPOSE" run --rm go-lint          # golangci-lint，基线 0 issue
"$COMPOSE" run --rm go-test          # go test -race -timeout=20m ./...
```

- golang:1.26 镜像没有 golangci-lint。
- lint 必须走 Dockerfile.test 的 go-lint 阶段。
- go-test 依赖健康的 mysql 与 postgres。
- 执行 compose run 时会自动拉起这两个数据库。
- 只跑纯 Go 单测时优先用一次性容器，更快更省。
- 单测命令加 -run TestName -v -count=1。
- 端到端验证用仓库脚本，不要手工拼命令。
- scripts 目录提供 full、functional 与 persistence 三套脚本。
- 完整门禁等价于 make check，需要 Go 与 Node 同时可用。

### 2.3 禁止事项

- 禁止用大阶段变更掩盖未完成分支或未验证行为。
- 禁止弱化、跳过或删除测试。
- 禁止重排或重命名无关代码。
- 禁止升级无关依赖。
- 禁止删除覆盖率。
- 禁止扩大兼容性白名单。
- 禁止改冻结的验证码行为。
- 保留工作区中无关的改动。

---

## 3. 提交与评审

### 3.1 提交颗粒度

- 一个任务含多个可独立验证的子任务时，必须拆成多条提交。
- 一条提交只做一件事，且该提交点自身可编译。
- 提交信息用中文，首行是类型加子任务简述。
- 正文写清改动点、为什么这样改、怎么验证。
- 禁止夹带无关的重构、格式化、依赖升级或调试残留。
- 新增测试与它测的改动放在同一条提交。

### 3.2 合并与适配器

- 任务完成后按 commit、merge、push 三步走。
- merge 使用 --no-ff，保留任务的整体边界。
- push 前先核实 remote。
- origin 通常指向上游仓库且无推送权限。
- 需要推送时先加自己有权限的 fork 作为 origin。
- 兼容适配器保留到调用方迁移完且契约测试证明可删。
- 紧急缺陷或安全修复可先于总计划，但须范围窄并记入。

```bash
cd "$REPO"
git merge --no-ff "<分支名>"
git push origin main
```

### 3.3 打 tag

- 命中以下任一条件即视为大的改进。
- 改变对外行为契约，例如 API、协议端点或表结构。
- 新增或修改自动化执行语义。
- 跨多个包的结构性改动。
- 修复已在线上真实发生过的故障。
- 大的改进在合并后必须打 tag。
- tag 名带 -local 后缀，与上游正式版本号区分。
- 打 tag 前确认已通过完整验证并记录回滚方式。

```bash
git tag -a "<版本>-local-<日期>" -m "<中文说明：解决了什么问题>"
```

---

## 4. 构建与发布

### 4.1 镜像

- Dockerfile.debian13 依次构建前端、编译后端、装配 Playwright。
- compose.override.yml 把 app 指向本地镜像并禁用拉取。
- .docker/playwright-runtime 为空时会联网下载 Chromium，很慢。

```bash
"$DOCKER" build -f Dockerfile.debian13 -t ydisks-xianyu-helper:local .
"$DOCKER" compose up -d app      # 用本地镜像重启 app
```

- buildx 报权限错误时不要反复重试。
- 改用 create、cp、commit 手工构造镜像。
- 手工建镜像后必须校验二进制一致。

```bash
"$DOCKER" tag ydisks-xianyu-helper:local ydisks-xianyu-helper:rollback-<日期>
CID=$("$DOCKER" create ghcr.io/christ9038/ydisks-xianyu-helper:latest)
"$DOCKER" cp /tmp/xianyu-server "$CID:/app/xianyu-server"
"$DOCKER" commit "$CID" ydisks-xianyu-helper:local
"$DOCKER" rm "$CID"
"$DOCKER" run --rm --entrypoint sh ydisks-xianyu-helper:local -c "md5sum /app/xianyu-server"
md5 -q /tmp/xianyu-server
"$DOCKER" compose up -d app
```

### 4.2 桌面打包

- 应用默认端口 59188。
- 参数 -addr :59188 会监听所有网卡。
- 桌面包把服务绑定到 127.0.0.1:59188。
- 桌面包保持服务与托盘为独立进程。
- 除非用户明确要求或确认无 Chromium，否则禁止加 -no-browser。
- Windows 安装服务并为当前用户启动托盘程序。
- Windows 安装器只给交互用户服务状态与启停权限。
- 安装后托盘操作不得再弹 UAC。
- 服务配置与删除仅限管理员。
- macOS 注册 server 与 tray 两个 LaunchAgent。
- macOS 托盘设 LSUIElement 为真，不出现在 Dock。
- Linux 包是按架构区分的 tar 归档。
- Linux install.sh 必须以 root 在同架构机器上运行。
- Linux 数据放在 /var/lib/ydisks-xianyu-helper。
- 所有桌面包都含匹配的 driver、Chromium 与 headless shell。
- 安装时禁止添加 Debian Chromium 包。
- 安装时禁止下载第二个浏览器。
- Docker 最终镜像基于 node:24-trixie-slim。
- 只通过自带 driver 安装 Chromium 系统库。
- 在同一镜像层清理 apt 索引与临时缓存。
- 托盘状态机由 Windows 与 macOS 共享。
- 托盘串行化动作并显示过渡状态。
- 托盘在启动或重启后等待健康检查通过。
- 托盘在停止后等待端点不可达。
- 托盘退出前先停止服务。
- 托盘提供打开日志目录的动作。
- 桌面首次初始化在 Web UI 完成。
- 用户在 127.0.0.1:59188 输入并确认管理员密码。
- -init-admin 是运维与无头环境的兜底。
- Docker Compose 用 XIANYU_ADMIN_PASSWORD 做非交互初始化。

### 4.3 macOS 打包

- macOS 安装包必须用 build-pkg.sh 构建。
- 禁止手工复制 Chromium 或 driver 到 dist。
- runtime 不完整时脚本会自动整理。
- prepare-runtime.sh 读取本机 Playwright 缓存目录。
- 缓存未就绪时先运行 browser-install。
- 不要只复制 chromium，还需同版本 headless shell。
- 打包前必须确认包内 runtime 能启动并通过健康检查。
- 无签名身份可出未签名 pkg，但必须告知未签名状态。

### 4.4 CI 与发布

- 禁止在 CI 中手工组装桌面包。
- 桌面工作流要构建前端并编译各平台二进制。
- 要恢复或填充对应架构的 Playwright runtime 缓存。
- 要用平台打包脚本装配并执行签名步骤。
- Windows 用 installer.iss。
- macOS 用 build-pkg.sh。
- Linux 打包含安装脚本、卸载脚本与 systemd 单元的归档。
- 桌面 CI 在 main 与 dev 上运行。
- 正式桌面构建由 release.yml 处理版本标签。
- Linux amd64 与 arm64 必须用原生 runner。
- 禁止使用 QEMU 或跨架构模拟。
- Docker 发布也在各架构原生 runner 上构建。
- 分支构建发布 main 或 dev 与 sha 标签。
- 正式构建通过审批后发版本标签、latest 与 sha 标签。
- 版本标签生成 GitHub Release 与 SHA-256 校验和。
- 全部架构的测试、Chromium 启动与健康检查通过前禁止发镜像清单。

---

## 5. 仓库与文档

- 主要目录是 cmd、internal、frontend 与 webui 静态产物。
- cmd 下有 server、init-admin 与 dbverify 等入口。
- internal 下有 server、adapter、account 与 engine。
- internal 下还有 automation、xianyu、browser 与 db。
- internal/adapter 负责系统事件、订单详情与凭证续期接线。
- internal/xianyu 内分 mtop、ws、qrlogin 与 protocol。
- frontend 是 React 与 Vite 源码。
- 构建产物嵌入 internal/webui/static。
- 运行时接线描述不构成往处理器加业务的许可。
- 生产账号恢复先走协议级续期，再要求扫码登录。
- 账号恢复禁止调用浏览器密码登录。
- 浏览器契约要与 server 和 engine 调用方保持一致。
- Vite 把后端路由代理到本机 59188。
- 前端构建用 npm --prefix frontend run build。
- 前端开发服务器用 npm --prefix frontend run dev。
- 改动源码后要重建前端，保证嵌入产物最新。
- 改 API 路由前缀要同步更新 vite 配置。
- 协议与数据库行为要保持聚焦测试覆盖。

---

## 维护本文件

- 一条规则一句话，每句不超过 50 字。
- 新规则放进对应阶段与性质的小节。
- 命令只放代码块，不写成段落。
- 改动规则要同步更新本文件，不留过期条目。
