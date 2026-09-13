# OpenAPI 契约收口重构总计划

## 唯一治理规则

本文是唯一阶段状态、顺序和验收依据。`refactoring-progress.md` 只保存旧六阶段最终提交与证据，不定义新阶段入口。旧六阶段成果是不可回退的已完成架构基线：应用服务、数据库、生命周期和 React feature 化均不重新实施，既有安全、依赖、注释、兼容和冻结 CAPTCHA 门禁继续有效。

本计划只有六个正式阶段，严格按 1 -> 2 -> 3 -> 4 -> 5 -> 6 执行。一个阶段是一个任务、一个评审单元和一条最终中文提交；阶段中不得创建中间提交、切片交付、提前更新状态或进入后续阶段。

## 目标与边界

唯一 HTTP 契约链路：`OpenAPI 3.1 -> 生成 TypeScript paths/types -> 类型化 HTTP client -> feature adapter -> UI model`。

`api/openapi.yaml` 是 `/api/v1/**` 与 `/health` 的唯一 HTTP 契约源；旧兼容路径不重复登记，继续由兼容矩阵和等价路由测试保护。生成的 `frontend/shared/api-contract/generated/schema.ts` 只读并提交。真实 handler 使用 `kin-openapi` 按同一规范校验。

契约校验采用非对称规则：OpenAPI 中前端必需字段缺失或类型错误失败；声明的可选字段类型错误失败、缺失允许；后端额外非敏感字段允许，且不会出现在生成的前端类型中。敏感字段泄漏仍由安全门禁和响应测试独立阻断。UI 派生模型、表单状态和兼容归一模型不是 HTTP DTO。对象默认允许额外属性；动态设置、账号通知绑定等动态对象显式声明 `additionalProperties` 值类型。每个 operation 必须有稳定 `operationId`、成功响应、统一错误响应、鉴权元数据和请求参数，不得用无约束 `{}`、`object` 或 `any` 代替已知业务字段。

## 当前状态

| 阶段 | 状态 | 严格结论 |
| --- | --- | --- |
| 既有架构基线：稳定性、组合根、生命周期、React、DB、质量收口 | 已完成（不可回退） | 历史实现和证据留在 `refactoring-progress.md`，不重做、不改写历史。 |
| 1. 契约基础设施与全路由登记 | 已完成 | OpenAPI、生成漂移和真实 Router 双向门禁已建立，未改变业务运行时形状。 |
| 2. 类型化客户端与登录账号主链路 | 已完成 | session、system、accounts、QR 风控和永久关闭的 password-login 已迁移并完成真实响应校验。 |
| 3. 查询、聊天与订单主链路 | 已完成 | dashboard、实际消费的 admin 摘要、chat、orders、刷新任务和 WebSocket 消息均已迁移并完成真实契约校验。 |
| 4. 商品、卡券和文件传输 | 已完成 | items、批量发布、cards 和上传 adapter 已迁移；原生 FormData、取消、重试和批次隔离均保留。 |
| 5. 自动化、设置和通知动态契约 | 已完成 | rules、automation、settings、notifications 和账号绑定均通过契约客户端；动态账号键有明确值类型约束。 |
| 6. 全量封闭与旧手写契约退场 | 已完成 | 已关闭原始 HTTP client 旁路，删除手写 transport DTO 和名单式门禁；全部 operation 已具备真实成功响应或明确特殊校验证据，契约门禁永久启用。 |

## 阶段一：契约基础设施与全路由登记

建立 `api/openapi.yaml`、固定版本的 `openapi-typescript`、`openapi-fetch`、`kin-openapi`、`make api-generate`、`make api-check` 和 CI 漂移检查。通过 `chi.Walk` 比较真实 Router 与规范，双向覆盖 `/api/v1/**`、`/health` 和动态订单刷新路由。每个 operation 立即登记稳定 ID、成功响应、统一错误响应、鉴权和路径参数；业务成功 schema 在后续阶段逐操作收紧。删除 `frontendDTOContractSpecs` 名单式门禁，保留二维码字段修复和通知摘要敏感字段保护，不改变任何 URL、状态码、包装或 feature 行为。

验收：

```text
make api-check
go run ./tools/architecturecheck
go test ./internal/server -count=1
npm run typecheck --prefix frontend
npm test --prefix frontend
make comments
npm run build --prefix frontend
git diff --check
```

最终提交：`阶段一：建立 OpenAPI 单一契约源与全路由门禁`。

## 阶段二：类型化客户端与登录账号主链路

把 Cookie、超时、外部 AbortSignal、401 合并登出、统一 ApiError 和 FormData 行为封装进类型化客户端运行时，迁移 session、system、accounts、QR 风控及永久关闭的 password-login。adapter 继续输出 UI model，生成类型不得直接进入 React state 或 props；补齐成功、未认证、越权、未找到、风控状态和敏感字段不泄漏的真实响应校验。冻结 CAPTCHA 文件、选择器、时序、Cookie 合并和浏览器调用顺序完全不变。

最终提交：`阶段二：迁移登录账号与风控接口到生成契约`。

## 阶段三：查询、聊天与订单主链路

迁移 dashboard、admin、chat、orders 查询和订单刷新任务。WebSocket 握手登记为 HTTP operation，消息体使用 OpenAPI component schema；聊天 adapter 保留唯一原生 WebSocket 实现。保留分页、游标、订单状态、旧包装归一化和晚到响应隔离，覆盖刷新成功、失败、取消、超时及消息字段类型。

最终提交：`阶段三：迁移查询聊天与订单接口到生成契约`。

## 阶段四：商品、卡券和文件传输

迁移 items、批量发布、cards、表格和图片上传、CSV 下载。OpenAPI 明确 multipart、二进制响应、Content-Disposition 和长请求超时；继续使用 FormData，保留批次代次隔离、取消、重试和不确定远端结果。覆盖格式错误、部分成功、取消、重试、CSV 和客户端取消。

最终提交：`阶段四：迁移商品卡券与文件接口到生成契约`。

## 阶段五：自动化、设置和通知动态契约

迁移 rules、automation、settings、notifications 和账号通知绑定。动态设置与按账号 ID 的动态键使用受约束 additionalProperties；通知摘要不返回 SMTP 密码、Token 或渠道秘密配置，编辑器不从摘要 DTO 恢复秘密。覆盖动态键类型、敏感配置三态变更、渠道别名归一、自动化问题和统一错误响应。

最终提交：`阶段五：迁移自动化设置与通知动态契约`。

## 阶段六：全量封闭与旧手写契约退场

禁止 feature、组件和 Hook 导入原始 `get/post/put/del/postForm`，只有共享契约客户端运行时可调用 fetch。删除手写 `transport.ts` 和废弃 DTO；门禁从 OpenAPI 自动发现 operation、生成类型和真实契约测试覆盖，不保留 DTO 名单、路径白名单或可扩展 baseline。每个 operation 必须有真实成功响应，或属于明确的 WebSocket/二进制特殊校验。更新 AGENTS、依赖规则、兼容矩阵和进度证据；新接口必须先改 OpenAPI、生成代码和服务端契约测试才能被前端使用。

最终验收：`make check`、`go test ./... -count=1`、Server race、全部前端测试、`make cover`、`make cover-frontend`、嵌入前端构建及二维码/浏览器回归。最终提交：`阶段六：完成生成契约迁移并永久关闭旁路`。

## 执行纪律

阶段最终验收失败时状态仍为当前阶段，修复后重跑完整命令；只有全部命令成功后才一次更新状态、证据和下一阶段入口并创建最终中文提交。六个阶段现已全部完成，后续不再开启新阶段入口。不得扩大白名单、baseline、忽略路径或 warning-only 旁路；后续缺陷修复保持全部已启用门禁永久有效。

## 后续窄范围安全修复记录

- 2026-09-13（发布前审查后续可靠性修复，已完成、本地未发布）：修复一次性默认回复状态写入连续失败后租约到期自动重发、批量发布长间隔等待消耗最终网络超时预算、商品详情探测丢失同步 Cookie 会话三项问题。新领取记录先持久化为不可自动接管的 `sending` 状态；最终发布请求在节流闸门返回后单独创建两分钟网络预算；全量和分页商品详情探测复用列表同步的 CookieSession，并刷新首阶段 Cookie 写回后的并发比较基准。新增 SQLite、MTOP、本地平台替身和同步会话回归测试。`make check`、`make cover`（Go statement 81.4%，未设置 RUN_BROWSER_INTEGRATION）、`make cover-browser`（RUN_BROWSER_INTEGRATION=1，浏览器包 64.2%）、`make cover-frontend`（89 文件 516 测试，statement 78.92%）、前端 typecheck/构建及新增场景定向 race 均通过；受影响包全量 race 因既有 `internal/db.TestOrderOwnershipRecoveryUnsafe` 在 10 分钟迁移压力测试超时未完成。真实账号、外部平台及 MySQL/PostgreSQL 验证未执行。

- 2026-09-12（发布前执行权与分页扩查，已完成、本地未发布）：修复普通自动化及补发在途续租和逐次副作用失权检查、恢复扫描旧快照覆盖新状态、删除后分页状态不同步、首页与追加分页交错、旧删除请求晚到清除新状态及商品发送中切换会话的忙碌状态残留。成功省略 `cardList` 一律按空列表处理，不再用分页或总数元数据否定该协议事实；显式数组类型及分页完整性检查保留。同步修正既有 MySQL 创建时间回归的驱动格式假设，解析后精确比较 UTC 时刻，保留空值和默认时间断言。`go test ./... -count=1`、最终 `make cover`（Go statement 81.4%，未设置 RUN_BROWSER_INTEGRATION）、`make cover-browser`（RUN_BROWSER_INTEGRATION=1，浏览器包 64.1%）、`make cover-frontend`（89 文件 512 测试，statement 78.90%）、完整 SQLite/MySQL 8.4/PostgreSQL 17 `make test-multidb`、自动化/数据库定向 race、新增租约回归重复 5 轮 race、`make test-server-race`、API/架构/comments/vet/lint、前端 typecheck 和构建、Go 构建、实际 Chromium 启动及健康检查和 SIGTERM 收口均通过。新增租约守卫及两项仓储操作 statement 100%。六阶段、HTTP/OpenAPI、数据库 schema、冻结 CAPTCHA 和注释基线不变；真实账号平台投递/发货/风控及各平台发布安装包未执行，详细边界见 [执行权与分页扩查记录](release-execution-state-fix-20260912.md)。


- 2026-09-12（发布前审查 P1/P2 后续修复，已实施、本地未发布）：修复人工补发失败清理清除 `action_started` 导致整批卡密重复消费；补发失败现在固定保留 `needs_review` 和未知结果保护，历史已有发送数量但缺少占用标志时禁止普通重试。模板执行器保留后续取卡失败前已发送的数量与凭证，并在加密快照中记录合法渲染为空的模板消息位置，补发完整性按 `prepared + unknown + skipped = expected` 校验，跳过位置重复、越界或历史证据不足时安全拒绝。聊天 Hook 在删除和账号切换时清理联系人分页忙碌状态、阻止删除期间新分页并保留代次隔离。新增自动化、数据库和前端回归测试；`go test ./... -count=1`、自动化/数据库定向 race、`make test-server-race`、前端 89 个文件 506 项测试、架构/API/vet/lint/comments、Go 81.3% 覆盖率、浏览器 64.2%（`RUN_BROWSER_INTEGRATION=1`）、前端 78.79% 覆盖率、服务 Chromium 启动 `/health` 与 SIGTERM 收口均通过。未修改冻结 CAPTCHA、商品列表省略 `cardList` 成功语义、HTTP/OpenAPI、账号凭证协议或数据库 schema；真实账号/外部平台、MySQL/PostgreSQL 和桌面包验证仍是环境例外。完整边界和证据见 [发货补发与聊天分页修复记录](release-reliability-fix-plan-20260912-replay-chat.md)。

- 2026-09-12（发布前 P1 后续修复，已实施、本地未发布）：本轮审查复现的“旧取消记录吞掉新 mid 成功回显”和“仅 sdkSilent 等无关凭证变化导致 Token 恢复误判、立即重连”已修复。消息观察在同一锁内完成归属、取消保护和 PNM 登记；凭证恢复只接受实际参与签名的非空 `_m_h5_tk` 前缀轮换，仓储读取失败不走兼容成功分支，并将连续失败期间的即时刷新限制为一次。新增正式回归与签名比较测试，`go test ./... -count=1`、P1 race 20 次、服务生命周期 race、架构/API/vet/lint/comments、前端检查及 Go 覆盖率 81.3% 均通过。详细实施边界、事件决策表、签名证据、测试矩阵和未执行的真实平台/三方数据库/桌面包验证见 [发布可靠性修复计划](release-reliability-fix-plan-20260912.md)。六阶段完成状态和已启用门禁保持不变，保留既有未提交变更与历史证据。

- 2026-09-11（发布前审查修复，已完成、本地未发布）：修复人工补发遗漏后续动作、补取结果持久化失败后重复取卡，以及完整 Cookie Jar 并发冲突检查遗漏。补发按原始任务计划核对完整性；新增加密快照中的补取占用标记，在外部取卡前持久化并由结果快照原子解除，异常恢复不能绕过。续期按完整作用域检查响应会覆盖的并发变化，保留无关增量合并。范围限定于自动化、凭证协议/适配和确定性回归；不修改冻结 CAPTCHA、HTTP/OpenAPI、数据库 schema 或注释基线。新增确定性回归覆盖未执行后续动作、规则编辑、规格变化导致补发跳过、占用和结果写入失败、未知发送结果、库存恢复失败、请求取消、恢复入口防绕过，以及 Cookie 域/路径/分区/属性/删除和重定向链冲突。历史人工运行测试夹具补齐生产运行已有的原始任务计划，未弱化成功与失败断言。最终 `make check`（全库 Go 测试、API/架构、vet、lint、中文注释）、`make test-server-race`、自动化/适配/续期定向 race 回归、服务构建和 `git diff --check` 均通过。`make cover` Go statement 81.3%（未设置 RUN_BROWSER_INTEGRATION）；新增计划校验和 Cookie 冲突判断函数 statement 100%，补取函数 91.4%，其剩余防御性计划拒绝、仓储读取错误和 API 卡密拒绝分支归为确定性覆盖补全工作，不列为环境例外。`make cover-browser` 浏览器包 64.0%（RUN_BROWSER_INTEGRATION=1）；`make cover-frontend` statement 78.76%（89 个文件、505 项测试）。最终二进制使用临时 SQLite 和默认浏览器启动，Chromium 就绪、/health 返回 200、SIGTERM 后退出码 0。真实账号 MTOP 续期、消息投递及平台确认发货属于外部平台例外，本次未调用；MySQL/PostgreSQL 未配置测试连接，各平台安装包未重建，属于环境验证缺口。无法核对完整原始计划或存在未收口补取的历史订单转人工核对，不声称自动恢复全部历史异常。覆盖率产物不提交，原有工作区修改保持不变。
- 2026-09-12（发布前审查 P1 修复，本地未发布）：修复首次自动发货消费数据卡后库存恢复失败仍允许人工补发再次取卡的问题；动作结果现在持久化 `RefillPending`，人工入口和恢复 worker 在任何外部动作前统一拒绝并转人工核对，模板批量卡密恢复失败也沿用同一保护。修复并发相同正文出站消息的回显错配；发送请求生成的 `mid` 通过上下文、WS 响应摘要和引擎等待器全链路传递，带 `mid` 的响应只唤醒同一请求，无 `mid` 的异步推送不会消费已关联请求。新增首次库存恢复失败、补取保护、同文响应乱序、同 PNM 推送竞态和请求 mid 透传回归测试；`go test ./... -count=1`、相关包 race 回归、`make architecture`、`make api-check`、`make vet`、`make lint`、`make comments`、前端类型检查/测试、服务构建及 `git diff --check` 均通过。未修改冻结 CAPTCHA、数据库 schema、HTTP/OpenAPI 或既有前端行为。

- 2026-09-11（本地修复，未发布）：修复发布前审查确认的六项问题。账号任务在凭证锁内比较请求开始或上次成功写回的 Cookie/metadata 版本，拒绝陈旧响应覆盖并发续期；先释放凭证锁再同步运行时，避免 UpdateCookie 重取同一锁造成死锁，仅 Cookie 属性变化也会通知运行时。文本、图片和商品卡片发送错误统一转换为 transport DTO，商品弹窗对 uncertain 消息展示人工核对提示并关闭本次发送流程，保留账号/会话与请求代次隔离。成功响应省略 cardList 时不再要求额外提供总数字段；明确非零总数/页数的矛盾响应及显式错误类型继续保留完整性检查，本条取代下方 2026-09-10 空商品修复记录中“缺少总数字段即拒绝”的限制。RunAccountTask 在解引用前检查初始化状态，消除 SA5011。新增锁重入、完整/扁平凭证并发冲突、连续写回、属性变化、存储失败、三类消息错误 DTO、uncertain 会话隔离及缺失总数字段的确定性回归；既有畸形商品测试将成功空对象移至成功覆盖范围，保留并补充显式错误类型断言。不修改冻结 CAPTCHA、数据库 schema、OpenAPI 路由或注释基线，前端嵌入资源已重建，六阶段完成状态和全部门禁不变。`make check`（含全库 Go 测试、API/架构、vet、lint、注释）、定向回归、`make test-server-race`、账号凭证回归 `go test -race ./internal/automation -run 'TestTaskCookie|TestRunAccountTaskUninitialized' -count=1`、前端类型检查及构建通过。`make cover` Go statement 81.2%（未设置 RUN_BROWSER_INTEGRATION），`make cover-browser` 浏览器包 64.1%（RUN_BROWSER_INTEGRATION=1），`make cover-frontend` statement 78.76%（89 个测试文件、505 项测试通过）。服务使用临时空 SQLite 数据库和默认浏览器配置启动，Chromium 就绪，/health 返回 200，SIGTERM 后正常退出。真实账号的 MTOP 续期、平台消息投递与实际商品擦亮未执行；MySQL/PostgreSQL 未配置测试连接，未重复验证三方言或各平台安装包。本次新增逻辑均由本地确定性夹具验证，覆盖率文件不提交。

- 2026-09-10（本地修复，未发布）：以当前账号 Cookie 和正常 Chrome 桌面 UA 实测确认，闲鱼商品列表在账号没有在售商品时返回 `SUCCESS`、`totalCount=0`、省略 `cardList`。商品分页协议层现在仅在平台明确总数为零、没有非零页数且确实省略 `cardList` 时归一为空页；`cardList:null`、错误类型、缺少总数字段或总数/页数非零仍视为不完整响应并拒绝同步，避免软删除本地商品。普通全量同步、分页同步与每日擦亮已移除专用分支，统一使用该规则；空商品全量同步会正常完成本地 reconcile，空擦亮任务正常完成当日记录。新增协议层、自动化当日去重和真实 `/api/v1/items/get-all-from-account` 空全集同步回归；不修改 HTTP/OpenAPI、数据库 schema、冻结 CAPTCHA、Cookie/Token 续期或平台重试上限。`make check`、`make cover`（Go statement 81.3%，未设置 `RUN_BROWSER_INTEGRATION`）、`make test-server-race`、`make cover-browser`（64.1%，`RUN_BROWSER_INTEGRATION=1`）、`make cover-frontend`（statement 78.74%）及 `git diff --check` 均通过；真实平台只执行了只读商品列表查询，未输出或持久化凭证。

- 2026-09-10（本地修复，未发布）：修复人工完整发货对已有内容快照并发补发时，两个请求可同时发送同一条买家消息的问题。失败或人工核对运行现在先以数据库条件更新领取唯一补发权并递增执行代次；仅领取者可以发送、释放或收口，未领取的并发请求直接拒绝。补发在发送或确认失败后恢复原终态；不改动快照内容、卡密领取、HTTP/OpenAPI、数据库 schema、冻结 CAPTCHA、Cookie/Token 流程或平台重试规则。SQLite 覆盖领取、旧代次拒绝、释放、再次领取和收口，并以阻塞发送器验证并发请求只发送一次；同时补齐协议续期测试中的中文变量注释。`go test ./... -count=1`、`make cover`（Go statement 81.2%，未设置 `RUN_BROWSER_INTEGRATION`）、`make test-server-race`、`go vet ./...`、架构/注释门禁及 `git diff --check` 均通过；未调用真实账号或外部平台。

- 2026-09-10（本地修复，未发布）：直接复核官方 auto-login SDK 并运行隔离脚本夹具，修复脚本可见 Cookie、同名末值与 URI 解码、日期边界、等待后分支选择、响应头 Promise 超时、严格成功 JSON、迟到响应不得恢复成功、成功 reload 条件、重定向 Cookie 链和 Max-Age 起算时间；生产 adapter 以最新 Jar 增量重放响应，避免并发旧快照覆盖，补齐裸 MTOP Session 失效码。保留用户确认的失效后立即协议续期增强，不修改冻结 CAPTCHA、HTTP/OpenAPI、数据库 schema、前端或注释基线。源码指纹、逐项行为、测试证据及不能承诺完整浏览器等价的边界集中记录在 `sdk-renewal-alignment-audit.md`；六阶段完成状态与全部门禁保持不变。

- 2026-09-10（本地修复，未发布）：补齐仅 MTOP 签名 Token 过期时的登录态恢复路径。WebSocket 连接、自动评价、擦亮、订单详情、确认发货、改价、商品发布、商品同步和批量发布在 Token 重试耗尽后，现在统一进入不受健康账号 `sdkSilent` 疲劳窗口影响的协议 Cookie 续期；Session 失效继续保留原有 Session 阻断与恢复语义。新增统一凭证失效分类和 Token 过期回归测试，未修改冻结 CAPTCHA、平台 Token 重试上限或数据库 schema。

- 2026-09-10（本地修复，未发布）：修复 MTOP Token 过期响应后的刷新判断。原逻辑以完整 Cookie 字符串变化误判签名 Token 已轮换，响应只更新普通 Cookie 时会跳过 `RefreshTokenContext` 并继续使用旧 `_m_h5_tk`；现在仅当 `_m_h5_tk` 真正变化才直接重试，否则立即调用官方 Token 接口刷新后重试。统一修正评价、商品、订单、确认发货、改价、聊天商品、账号资料和发布请求路径，并补充评价接口“普通 Cookie 变化 + Token 过期”回归测试。未修改冻结 CAPTCHA、业务重试上限或数据库 schema。

- 2026-09-10（本地修复，未发布）：修复自动评价和每日擦亮任务在 MTOP 请求失败路径丢失响应 Cookie 的问题。任务现在使用完整 Cookie 快照会话执行平台请求，成功和失败返回都会收口响应 Set-Cookie；带 Domain/Path/HttpOnly 的账号写回时保留 metadata 快照，不再退化为扁平 Cookie。若官方 Token 接口返回 `accessToken` 但没有真正轮换 `_m_h5_tk`，任务立即保留 Token 过期分类并交给上层恢复，不重复发送旧签名。补充完整快照写回和无签名轮换回归测试；未修改冻结 CAPTCHA、HTTP/OpenAPI、数据库 schema 或平台重试上限。

- 2026-09-10（本地修复，未发布）：为账号自动评价和商品擦亮任务增加持久化执行次数与最大重试次数。首次执行失败后最多自动或人工重试 5 次，第 6 次执行失败会保留失败状态但清除 `next_retry_at`，调度器不再重复领取；人工入口达到上限时返回明确限制错误。三方言新增对齐迁移和 SQLite 上限回归测试；空商品列表成功态仍不消耗重试次数。未修改 HTTP/OpenAPI、冻结 CAPTCHA、Cookie/Token 流程或平台重试/重连规则。

- 2026-09-10（本地修复，未发布）：修复每日擦亮把“商品列表接口成功但返回 0 个在售商品”误判为失败并每 10 分钟重试的问题。空列表现在写入当日擦亮完成日期、运行记录为 `success` 并记录 Info，不再重复请求；商品列表接口错误、Cookie 持久化失败和实际商品擦亮失败仍分别保留失败/人工核对或延迟重试语义。补充 SQLite 当日去重回归、空列表完成标记失败和流程分支测试；未修改 HTTP/OpenAPI、数据库 schema、冻结 CAPTCHA、Cookie/Token 流程或平台重试/重连规则。

- 2026-09-09（本地修复，未发布）：修复评价后发送赠品在生产 v1.0.10 中仅以 WebSocket 写入成功作为成功条件的问题。自动化文本和图片发送现在会在写入前登记、等待账号自身 WebSocket 回显；回显按会话、消息类型和实际正文匹配，图片回显兼容嵌套协议正文。确认窗口内未收到回显时返回不确定结果并进入人工核对，不自动重试、不重新消费卡密；人工聊天保持原有非阻塞发送语义。补充回显确认、超时、分发唤醒及图片解析测试；未修改 HTTP/OpenAPI、数据库 schema、冻结 CAPTCHA、Cookie/Token 流程或平台重试/重连规则。

- 2026-09-09（本地修复，未发布）：修复闲鱼已由官方确认发货、但本系统尚未向买家发送内容时，人工完整发货被自动运行共用幂等键错误阻断的问题。人工完整发货改用订单级独立幂等键；自动运行零发送且结果确定的失败不再阻断人工补发。订单付款发货链路将按消息顺序加密保存实际发送的文本和图片，成功后保留快照；失败或人工核对状态再次发货时仅按快照重发，不重新取静态卡、消费数据卡或请求 API 卡密。此前 API 卡密接口结果不确定而没有快照的运行明确停止并提示核对，防止重复扣费。普通通知、邀评等非订单付款消息不持久化发货快照。未修改 HTTP/OpenAPI、数据库 schema、冻结 CAPTCHA、Cookie/Token 流程、平台重试/重连规则、门禁或注释基线。定向人工发货、快照重发、官方确认、未知 API 卡密和调度回归，`make comments`、`git diff --check`、`make test-server-race`、全库 `go test ./... -count=1` 及 `make cover` 均通过；Go statement 覆盖率 81.1%，未设置 `RUN_BROWSER_INTEGRATION`，未调用真实账号或外部平台。

- 2026-09-08 至 2026-09-09（本地修复，未发布）：修复 GitHub #36–#39 及审查发现的 #37 消息时序与重复查询问题。按用户要求移除人工插入订单 UI 并将新旧导入接口统一停用；会话创建时优先用本地商品确定买卖角色，旧会话仅在角色未知时执行一次商品发布人核验并持久化，后续消息不重复查询，且消息落库和广播先于外部核验；修复新版付款卡片被误判为旧简化消息导致漏发货；商品全量同步拒绝分页截断、重复和畸形列表，账号级付款发货规则必须明确确认适用于全部商品。新增三方言 00047 会话角色迁移，聊天 API 与打包前端统一使用对端语义字段；同步更新 OpenAPI、生成类型、兼容说明和嵌入资源。未修改冻结 CAPTCHA、Cookie/Token 流程、平台重试/重连规则、门禁或注释基线。行为边界、验证命令、覆盖率及真实平台未验证范围集中记录于 `issues-36-39-fix.md`；六阶段完成状态不变。

- 2026-09-06（本地修复，未发布）：修复 MTOP 错误响应调整造成的五项错误处理回归。确认发货普通业务失败恢复为 `ok/ret` 确定性结果，避免被 automation 当作远端动作不确定并保留非 2xx `FAIL_BIZ_*` 的业务语义；登录状态和 Token 刷新先处理可恢复的 Session、Token 和风控 ret，非 2xx 风控保留验证 URL；发布流程仅依据类型化 Token 错误映射 `auth_expired`，不再把刷新阶段网络、HTTP、取消或风控错误误报为认证过期；`MTopResponseError` 增加底层错误链，保留 `errors.Is/errors.As` 能力且不泄露敏感文本。补充真实本地 HTTP 响应、上层调用契约、非 2xx 状态、验证码 URL、发布错误分类、JSON 解析错误链和脱敏断言。未修改冻结 CAPTCHA、HTTP/OpenAPI、数据库或前端行为；架构门禁、注释门禁和历史基线保持不变。`make check`、全库 `go test ./... -count=1`、`make cover` 通过；Go statement 覆盖率 81.5%，MTOP 包 89.9%，无真实账号和外部平台调用例外。`golangci-lint` 报告 0 issues，仅保留仓库外 `Ydisks-Xianyu-Helper-multi-spec` worktree 文件缺失导致的既有 generated-file-filter warning。

- 2026-09-05（本地修复，未发布）：修复再次审查确认的两项订单同步回归。恢复写入把平台 `unknown` 当作未提供状态，防止同账号软删除恢复和历史错绑修正覆盖本地已完成状态；会话匹配始终同时查询裸买家标识与 `@goofish` 后缀，保留候选歧义及账号、商品隔离。补充真实 SQLite 恢复和重复同步测试、状态转换单测及三方言会话匹配用例。无需迁移或契约变更，冻结 CAPTCHA、六阶段状态、注释基线和架构门禁不变；本轮验证记录见 `order-sync-ownership-fix-plan.md` 第 8 节。

- 2026-09-05（本地修复，未发布）：修复未提交内容审查确认的五项回归。规则删除保护补齐部分发送后的 `[safe_retry]` 与 `action_started` 未知结果，判定与 00043 升级清理一致；联系人分页允许向历史递减，按已见游标及页数预算阻止停滞和循环；订单同步在凭证锁外补联系人，重新持锁后复核取消、凭证及发现代次，再执行本地提交。通知编辑在脱敏配置加载完成后才开放表单，并隔离关闭、新建和切换后的旧响应；兼容旧版 SMTP 覆盖，默认通过新增可选 `email_recipient` 更新语义保留所有服务端 SMTP 字段及秘密，仅在用户选择重新配置时替换完整配置。同步更新 OpenAPI、生成类型、真实 handler 契约、前端适配器及嵌入资源。六阶段状态、冻结 CAPTCHA、白名单和注释基线保持不变。验证命令、覆盖率和已知基线例外见 `order-sync-ownership-fix-plan.md` 第 7 节。

- 2026-09-04（已完成，本地已应用，未发布）：补齐已发布版本软删除自动化规则的升级清理。新增三方言 `00043_deleted_automation_rules_cleanup`，仅清理没有待处理运行的历史已删除规则，依靠现有外键级联删除其动作、模板绑定及已结束运行；卡密、订单、延期任务和 00042 独立执行守卫不删除。保留启用或仅停用的未删除规则，以及 running、needs_review、action_started 结果未知、普通可重试及部分发送后的 safe_retry 运行。不改变现行规则删除接口或冻结 CAPTCHA；六阶段状态、全部门禁和注释基线保持不变。定向 `go test ./internal/db -run 'TestMultiDB_DeletedAutomationRules|TestMultiDB_TargetMatrix' -count=1 -v`、严格 `make test-multidb`、旧迁移/规则删除回归、`make check`、注释、`git diff --check` 和服务构建通过；`make cover` Go statement 81.5%（未设置 RUN_BROWSER_INTEGRATION），`make cover-browser` 浏览器包 64.2%（RUN_BROWSER_INTEGRATION=1），`make cover-frontend` 前端 statement 79.19%。新增 SQL 业务分支均有 SQLite 确定性夹具，并使用 Docker 中的 MySQL 8.4、PostgreSQL 17 完成严格三方言实测；本修复无真实平台接口调用和新增真实账号测试例外。真实数据库副本经 `cmd/dbverify` 升级通过，3 组卡密逐一 DELETE 成功后回滚。停止旧服务后的 一致性备份为 `/tmp/xianyu-card-upgrade.0mhBkf/pre-restart.db`（私有目录、文件权限 0600，临时目录不是长期备份）；新服务启动自动推进至 43，清理 5 条旧规则及其 10 条终态运行，两类卡密引用均为 0。3 组卡密逐列比对备份无修改，18 条订单及 10 条独立执行守卫保留，外键检查和 `/health` 正常。数据清理不可逆，Down 仅回退迁移账本，不重建历史规则或运行；恢复必须使用升级前备份。新逻辑随下一版本分发，不需要用户提供账号/规则 ID 或执行手工 SQL；未结束运行继续受保护，不强行解除其引用。

- 2026-09-04（已完成，未发布）：修复买家侧 WS 评价误建卖家订单，以及历史错误归属导致一键同步整批失败。提供不依赖固定账号/订单的通用历史恢复；三方言 `00042_order_ownership_repairs` 保存非敏感修复审计、独立自动化执行痕迹及相关索引，保留普通写入归属 CAS、已有规则物理删除与通知变更；补齐完整分页、失败提示、恢复计数、凭证复核、账号发现代次和动作领取并发保护。真实已登录账号首次人工同步修正 2 单，卖家恢复到 12 单，重复及最终服务重启后人工同步均 0 失败、0 重复修正，自动化运行保持 10 条。最终 `make check`、注释、API、构建、`make cover`、`make cover-browser`、前端覆盖率及定向 race 通过；另以 Docker 中的 MySQL 8.4、PostgreSQL 17 连同 SQLite 完成严格 `make test-multidb`，全部 `TestMultiDB_*` 通过；普通 Go 语句覆盖率 81.5%（未设 RUN_BROWSER_INTEGRATION），浏览器包 64.2%（RUN_BROWSER_INTEGRATION=1），前端 79.19%；全库 DB race 一次十分钟超时不记通过，真实新交易断线流程未实测。所有证据、备份、恢复边界及例外集中记录于 `order-sync-ownership-fix-plan.md` 第 6 节。本次不新增上线自动同步、不改变冻结 CAPTCHA、不扩展白名单或注释基线；六阶段完成状态及全部门禁保持不变。

- 2026-09-01：通知外部请求错误在渠道边界统一移除 URL 用户信息、路径、查询参数和片段，HTTP 测试接口改为稳定公开错误，日志与 outbox `last_error` 仅保存脱敏诊断；旧 SHA-256 密码升级改为以用户标识和已验证摘要为条件的比较并交换写入，哈希、写入及驱动结果错误全部向认证层传播，避免并发改密被过期登录覆盖。本修复不改变 HTTP schema、数据库 schema、迁移编号、包边界或冻结 CAPTCHA 行为，也未新增白名单和 baseline。聚焦回归覆盖通知 Token/Webhook 泄漏、日志与持久化边界、并发改密、bcrypt 生成失败、数据库写入失败及受影响行数读取失败；`make check`、`go test ./... -count=1`、`make test-server-race`、`make comments` 和 `git diff --check` 通过，`make cover` 的 Go statement 覆盖率为 81.4%，`TestMultiDB_LegacyPasswordUpgradeCAS` 在 SQLite、MySQL 8.4 与 PostgreSQL 17 实测通过。额外执行的完整 `make test-multidb` 仅有与本修复无代码交集的既有 MySQL `TestMultiDB_OrdersUpsertManyMixedCreatedAt` 时间格式断言失败，SQLite、PostgreSQL 对应子测试及其余三方言用例通过；该基线问题未混入本次窄范围安全修复。
