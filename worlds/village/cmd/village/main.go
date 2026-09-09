// Agent Village 独立入口：一个干净的 AgentWorld Runtime，每位访客拥有自己的 Willow Creek（AV-01）。
//
// 运行方式（在项目根目录）：
//
//	go run ./worlds/village/cmd/village
//	# 打开网页: http://localhost:19200 （前端内嵌于二进制，无需单独构建）
//
// 环境变量：
//
//	VILLAGE_DB          SQLite 路径，默认 village.db
//	VILLAGE_SNAPSHOTS  每游客世界快照目录，默认 village_worlds（每游客一个 <uid>.json）
//	VILLAGE_ADDR        服务地址，默认 :19200
//	VILLAGE_INTERVAL    调度/世界 tick 间隔，默认 5s（= 推进 VILLAGE_SPEED 个游戏分钟）
//	VILLAGE_SPEED       每 tick 推进的游戏分钟，默认 10
//	VILLAGE_DAILY_ACTS  玩家每日行动额度，默认 20（对话/建议/赠礼/抉择各计 1 次；0 = 不限制）
//	LLM_API_KEY         （可选）启用玩家对话 LLM；不配置则纯模板回复，零 token 成本
//	LLM_BASE_URL        （可选）LLM 端点（默认 DeepSeek）
//	LLM_MODEL           （可选）模型名
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"agentworld/internal/agent"
	"agentworld/internal/bus"
	"agentworld/internal/db"
	"agentworld/internal/llm"
	"agentworld/worlds/village"
)

func main() {
	dbPath := envOr("VILLAGE_DB", "village.db")
	snapDir := envOr("VILLAGE_SNAPSHOTS", "village_worlds")
	addr := envOr("VILLAGE_ADDR", ":19200")
	interval := 5 * time.Second
	if v := os.Getenv("VILLAGE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		}
	}
	speed := 10
	if v := os.Getenv("VILLAGE_SPEED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			speed = n
		}
	}
	dailyActs := 20
	if v := os.Getenv("VILLAGE_DAILY_ACTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			dailyActs = n
		}
	}

	if err := os.MkdirAll(snapDir, 0755); err != nil {
		log.Fatalf("[village] 创建快照目录失败: %v", err)
	}

	// 1) SQLite（Agent 元数据：调度需要；每游客的 Agent 以 uid 前缀区分）
	d, err := db.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("[village] open db: %v", err)
	}
	sqlDB, _ := d.DB()
	if sqlDB != nil {
		defer sqlDB.Close()
	}

	// 2) LLM 客户端（未配置 Key = 模板回复，零 token）
	llmClient := llm.New(
		envOr("LLM_BASE_URL", "https://api.deepseek.com/v1"),
		os.Getenv("LLM_API_KEY"),
		envOr("LLM_MODEL", "deepseek-chat"),
	)

	// 3) 共享 Runtime（多世界共存：按 Agent.World 分派到对应 Module）
	brk := bus.NewBroker()
	rt := agent.NewRuntime(d, llmClient, brk)

	// 4) Hub：每游客一个独立世界，工厂按 uid 惰性创建（DB Agent + World + Module + 调度 + 时钟）
	hub := village.NewHub(func(uid string, ctx context.Context) (*village.Instance, error) {
		return village.NewInstance(uid, ctx, d, llmClient, rt, interval, speed, dailyActs, snapDir)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.SetCtx(ctx)

	// 5) 定时自动保存所有世界（容错：崩溃也不丢太多进度）
	go func() {
		tk := time.NewTicker(60 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				hub.SaveAll()
			}
		}
	}()

	// 6) HTTP + SSE + 内嵌前端
	srv := village.NewServer(hub)
	go func() {
		if err := srv.Start(addr); err != nil && err.Error() != "http: Server closed" {
			log.Printf("[village] HTTP 服务启动失败: %v", err)
			cancel()
		}
	}()

	if llmClient.Enabled() {
		log.Printf("[village] 玩家对话 LLM 已启用（%s）", llmClient.ModelName())
	} else {
		log.Printf("[village] 未配置 LLM_API_KEY：村民决策走规则 Planner，对话走模板（零 token 成本）")
	}
	log.Printf("[village] Willow Creek 已开放：每位访客拥有独立世界，速度 %d 游戏分钟/tick（%v/tick）", speed, interval)
	log.Printf("[village] 打开网页: http://localhost%s", addr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("[village] 收到退出信号，保存所有世界快照…")
	hub.SaveAll()
	cancel()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
