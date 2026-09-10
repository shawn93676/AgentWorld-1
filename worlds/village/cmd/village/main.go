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
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"agentworld/internal/agent"
	"agentworld/internal/bus"
	"agentworld/internal/db"
	"agentworld/internal/life"
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

	// 跨进程 Life Registry：登记各 World 的 Endpoint，使本进程发起 Travel 时能按 WorldKey 寻址。
	// 需在 Hub 工厂之前声明，供 Planner 注入的 Mover 闭包捕获。
	lifeReg := life.NewRegistry()
	lifeReg.SetEndpoint("village", envOr("VILLAGE_LIFE_URL", "http://localhost:19200/life"))
	lifeReg.SetEndpoint("economy", envOr("ECO_LIFE_URL", "http://localhost:19301/life"))

	// 4) Hub：每游客一个独立世界，工厂按 uid 惰性创建（DB Agent + World + Module + 调度 + 时钟）
	hub := village.NewHub(func(uid string, ctx context.Context) (*village.Instance, error) {
		inst, err := village.NewInstance(uid, ctx, d, llmClient, rt, interval, speed, dailyActs, snapDir)
		if err == nil && inst != nil {
			// M9-A：注入跨世界出行回调。Village 不持有 Economy 代码，只经 life.Registry
			// 的 Endpoint 发起 MoveRemote；Planner 自主决定跨世界时由这里真正执行。
			inst.World.SetMover(func(localID int64, toWorld string) error {
				ep, ok := lifeReg.Endpoint(toWorld)
				if !ok {
					return fmt.Errorf("no endpoint registered for world %q", toWorld)
				}
				_, merr := life.MoveRemote(inst.Adapter, ep, life.StableID("village", localID))
			return merr
			})
		}
		return inst, err
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
	srv := village.NewServer(hub, lifeReg)
	go func() {
		if err := srv.Start(addr); err != nil && err.Error() != "http: Server closed" {
			log.Printf("[village] HTTP 服务启动失败: %v", err)
			cancel()
		}
	}()

	// 演示模式：预创建演示世界（含铁匠 Marcus），供跨进程 Travel 扫描直接驱动。
	if u := os.Getenv("VILLAGE_DEMO_UID"); u != "" {
		if _, err := hub.Get(u); err != nil {
			log.Printf("[village] 预创建演示世界失败: %v", err)
		} else {
			log.Printf("[village] 已预创建演示世界 uid=%s（铁匠 Marcus 就位）", u)
		}
	}

	// M9-A：跨世界出行改由 Village Planner 自主决策（见 village.decide 的跨世界分支），
	// 不再由本处的定时器扫描强制触发。下方的 scanVillageTravel 已废弃移除。

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

// scanVillageTravel 已废弃（M9-A）：跨世界出行现由 Village Planner 在 decide() 中自主决策，
// 经注入的 World.mover 真正发起 MoveRemote，不再由外部定时器扫描强制触发。

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
