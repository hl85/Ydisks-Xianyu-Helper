# 核心链路覆盖率清单

> 本文件是 `AGENTS.md` §2.4「核心链路回归门槛」的执行清单：登记哪些文件属于核心链路、
> 当前语句覆盖率、以及无法覆盖分支的例外。任何代码改动合入前都要按本清单核对。

## 统计口径

- 覆盖率 = Go 语句覆盖率（`go test -coverprofile`，`-covermode=set`）。
- 统计必须显式列出核心链路全部包；跨包调用只有把被调包加入统计才会被计入。
- 报告是生成文件，禁止提交（`cover*.out` 已被 `.gitignore` 忽略）。

```bash
DOCKER=/Applications/Docker.app/Contents/Resources/bin/docker
REPO=/Users/wanghui/Code/oss-research/Ydisks-Xianyu-Helper
"$DOCKER" run --rm -v "$REPO":/src -w /src -v ydisks-gomod:/go/pkg/mod \
  golang:1.26 sh -c 'go test -coverprofile=cover-core.out \
    ./internal/automation ./internal/engine ./internal/adapter ./internal/db ./internal/xianyu/ws \
    && go tool cover -func=cover-core.out | tail -1'
```

## 核心链路定义

「自动化发货 / 回复主链路」= 平台事件或调度扫描 → 运行创建 → 动作执行 → 消息发送 →
自身回显/受理凭证确认 → 订单状态与运行状态落库，以及这条链上的持久化与协议层。

最近一次统计（2026-09-13，`go1.26.8` 容器，未启用 `RUN_BROWSER_INTEGRATION`）：

| 文件 | 链路职责 | 未覆盖/总语句 | 覆盖率 | 门槛 |
|---|---|---|---|---|
| internal/xianyu/ws/sync.go | 发送协议与受理凭证 | 0 / 167 | **100.0%** | 达标 |
| internal/engine/outgoing_message_coordinator.go | 出站消息发送 | 2 / 135 | **98.5%** | 差 1 个例外分支 |
| internal/engine/outgoing_echo_confirmation.go | 自身回显确认 | 1 / 74 | **98.6%** | 差 1 个例外分支 |
| internal/automation/run_coordinator.go | 运行编排、不确定语义 | 23 / 211 | **89.1%** | 未达标（余项全为存储故障分支） |
| internal/automation/scheduler.go | 计划任务扫描、待发货兜底 | 38 / 287 | 86.8% | 未达标 |
| internal/automation/center.go | 自动化中心入口 | 25 / 218 | 88.5% | 未达标 |
| internal/automation/action_executor.go | 各类动作执行 | 26 / 280 | 90.7% | 未达标 |
| internal/automation/manual_delivery.go | 人工补发 | 49 / 148 | 66.9% | 未达标 |
| internal/automation/card_delivery.go | 卡密发货 | 22 / 89 | 75.3% | 未达标 |
| internal/engine/dispatch.go | 消息分发与回显识别 | 21 / 295 | 92.9% | 未达标 |
| internal/engine/reply.go | 回复链 | 13 / 99 | 86.9% | 未达标 |
| internal/adapter/adapter_events.go | 事件接线 | 82 / 364 | 77.5% | 未达标 |
| internal/db/automation_recovery.go | 运行恢复与兜底查询 | 45 / 328 | 86.3% | 未达标 |
| internal/db/automation_delivery_proof.go | 发货凭证持久化 | 21 / 68 | 69.1% | 未达标 |
| internal/db/automation.go | 规则与运行仓储 | 39 / 245 | 84.1% | 未达标 |

包级汇总：automation 88.5%、engine 89.3%、adapter 83.7%、db 82.1%、xianyu/ws 88.8%。

### 与门槛的差距（按优先级）

1. `adapter/adapter_events.go`（82 条）：滑块验证恢复与协议续期链，需要浏览器与续期服务替身。
2. `scheduler.go`（38 条）：恢复任务与延迟任务的存储故障收口分支。
3. `manual_delivery.go`（49）、`card_delivery.go`（22）、`db/automation_delivery_proof.go`（21）、`db/automation.go`（39）：发货持久化细节。
4. `center.go`（25）、`action_executor.go`（26）、`dispatch.go`（21）、`reply.go`（13）。

## 例外登记

只允许三类例外：**可证明不可达**、**仅外部环境**、**需要真实平台账号**。
每条必须写明文件、行号、原因与最近复查日期。

| 文件:行号 | 语句数 | 类别 | 原因 | 复查日期 |
|---|---|---|---|---|
| internal/engine/outgoing_message_coordinator.go:203-206 | 2 | 可证明不可达 | 对仅含 string 字段的结构体做 `json.Marshal` 的 error 分支；Go 编码器对该形态不可能返回错误 | 2026-09-13 |
| internal/automation/scheduler.go:248-250 | 1 | 可证明不可达 | 兜底扫描的账号状态读取失败分支：`orders.cookie_id` 受外键约束必须指向真实账号，扫描查询与门禁读取之间不存在能产生读错误的窗口 | 2026-09-13 |
| internal/automation/run_coordinator.go:52-54 | 1 | 可证明不可达 | `prepareRuleRun` 所有 `run=nil` 的返回路径都同时置 `skipped=true`，`executeRule` 的空运行保护无法触达 | 2026-09-13 |
| internal/automation/run_coordinator.go:118-122,141-152 | 7 | 仅外部环境 | 人工核对/求评价计数落库失败分支：需在动作成功与状态写入之间制造存储故障 | 2026-09-13 |
| internal/automation/run_coordinator.go:341-348,358-361,369-371,395-401,413-416 | 13 | 仅外部环境 | 检查点推进与人工核对收口的存储故障分支：`StartRunAction` 与后续写入之间需数据库中途故障 | 2026-09-13 |
| internal/db/automation_recovery.go:582-584,693-695 | 2 | 仅外部环境 | `rows.Err()` 迭代中断分支，需要数据库连接在扫描中途物理损坏才能触发 | 2026-09-13 |
