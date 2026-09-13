// heartbeatcheck 是一个只读宿主侧检查入口：读取进程心跳表最近一次写入时间，
// 判断是否仍在容忍窗口内，并据此返回退出码，供 Docker healthcheck / systemd / 运维脚本直接使用。
//
// 本程序只执行 SELECT（db.Open 的迁移为幂等只读语义，已是最新版本时为空操作），
// 绝不发起任何平台（闲鱼）请求。退出码约定：0=健康；1=心跳过期或尚未配置；
// 2=无法确定（数据库连接或读取失败）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"xianyu-go/internal/db"
	"xianyu-go/internal/heartbeat"
	"xianyu-go/internal/logsafe"
)

// defaultHeartbeatCheckDBPath 是未指定数据库地址时的默认 SQLite 文件路径，与服务器默认路径一致。
const defaultHeartbeatCheckDBPath = "data/xianyu_data.db"

// main 是只读心跳检查 CLI 的进程入口；参数与结果统一由 runHeartbeatCheck 返回退出码。
func main() {
	// dbURL 是数据库连接地址命令行覆盖。
	var dbURL string
	// timeoutSec 是心跳容忍窗口（秒）。
	var timeoutSec int
	// instanceKey 是进程实例键，需与服务器配置一致。
	var instanceKey string
	flag.StringVar(&dbURL, "db-url", "", "数据库连接 URL（sqlite:// mysql:// postgres://）；缺省读 DATABASE_URL，再缺省用 data/xianyu_data.db")
	flag.IntVar(&timeoutSec, "timeout", 90, "心跳容忍窗口（秒）：超过该时长无新心跳即判过期")
	flag.StringVar(&instanceKey, "instance", "default", "进程实例键，需与服务器 XIANYU_HEARTBEAT_INSTANCE_KEY 一致")
	flag.Parse()

	// resolvedURL 按「命令行 > DATABASE_URL > 默认路径」优先级确定数据库地址。
	resolvedURL := strings.TrimSpace(dbURL)
	if resolvedURL == "" {
		resolvedURL = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if resolvedURL == "" {
		resolvedURL = defaultHeartbeatCheckDBPath
	}
	// timeout 是把秒数换算成的心跳容忍窗口；非正数视为非法配置。
	timeout := time.Duration(timeoutSec) * time.Second
	if timeoutSec <= 0 {
		fmt.Fprintln(os.Stderr, "错误：-timeout 必须为正整数秒")
		os.Exit(2)
	}
	// last、configured、err 保存只读检查的结果与错误。
	last, configured, err := runHeartbeatCheck(resolvedURL, instanceKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "心跳检查错误: %s\n", logsafe.Error(err))
		os.Exit(2)
	}
	// now 是本次判定的基准时刻。
	now := time.Now()
	// stale、_ 是最近心跳是否已超过容忍窗口。
	stale, _ := heartbeat.EvaluateHeartbeat(last, now, timeout)
	if !configured {
		// 数据库尚无任何进程写入心跳：可能未启用心跳或进程未启动，按不健康处理。
		fmt.Printf("UNCONFIGURED 数据库尚未有任何进程写入心跳（instance=%s），可能未启用心跳或进程未启动\n", instanceKey)
		os.Exit(1)
	}
	if stale {
		// 最近心跳已超出容忍窗口：宿主应判定进程停滞并介入（重启/告警）。
		fmt.Printf("STALE 进程心跳已过期：最近一次 %s，距今 %s，超过容忍窗口 %s（instance=%s）\n",
			last.UTC().Format(time.RFC3339), now.Sub(last).Round(time.Second), timeout, instanceKey)
		os.Exit(1)
	}
	// 心跳在窗口内：进程存活，健康。
	fmt.Printf("OK 进程心跳正常：最近一次 %s，距今 %s，容忍窗口 %s（instance=%s）\n",
		last.UTC().Format(time.RFC3339), now.Sub(last).Round(time.Second), timeout, instanceKey)
	os.Exit(0)
}

// runHeartbeatCheck 打开数据库并只读读取本实例最近一次心跳，返回最近时刻与是否已有记录。
// 打开阶段会执行幂等迁移（已是最新版本时为空操作），不修改任何业务数据；
// 连接或读取失败返回错误，由调用方按「无法确定」处理。
func runHeartbeatCheck(url, instanceKey string) (time.Time, bool, error) {
	// ctx 限制数据库打开与读取的取消预算。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// database、dialect、openErr 是打开数据库的结果。
	database, dialect, openErr := db.Open(ctx, url)
	if openErr != nil {
		return time.Time{}, false, fmt.Errorf("打开数据库失败: %w", openErr)
	}
	defer database.Close()
	// store 是只读检查用的心跳仓储；只执行 SELECT。
	store := db.NewHeartbeatStore(database, dialect, instanceKey)
	// last、readErr 是读取本实例最近心跳的结果。
	last, readErr := store.Latest(ctx)
	if readErr != nil {
		return time.Time{}, false, fmt.Errorf("读取心跳失败: %w", readErr)
	}
	// configured 标记数据库是否已有本实例心跳记录。
	configured := !last.IsZero()
	return last, configured, nil
}
