// Economy World 独立入口：起一个干净的 AgentWorld Runtime，只跑经济世界。
//
// 运行方式（在项目根目录）：
//	go run ./worlds/economy/cmd/economy
//
// 环境变量：
//	ECO_DB         经济世界数据库路径，默认 economy.db
//	ECO_INTERVAL   唤醒间隔，默认 3s
//	ECO_OBS_ADDR   观察服务地址，默认 :19100
//	ECO_TICK       世界需求刷新间隔，默认 5s（世界自己产生新工作/价格波动）
//	LLM_API_KEY    （可选）启用 LLM 决策；不配置则 20 个 Agent 走规则 Planner
//	LLM_BASE_URL   （可选）LLM 端点
//	LLM_MODEL      （可选）模型名
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gorm.io/gorm"

	"agentworld/internal/agent"
	"agentworld/internal/bus"
	"agentworld/internal/db"
	"agentworld/internal/life"
	"agentworld/internal/llm"
	"agentworld/internal/models"
	"agentworld/internal/scheduler"
	"agentworld/sdk"
	"agentworld/worlds/economy"
	ec "agentworld/worlds/economy/economy"
	"agentworld/worlds/goosegame/goose"
	"net/http"
	"sync"
)

// 经济世界角色（名字/职业/性格/初始资产）由 economy.InitialProfiles 定义。
// 这里只创建 Agent 的持久元数据（名字/职业/性格/目标）。

// travelers 记录通过 /life/enter 真正“跨世界抵达”本世界的 Agent（稳定的跨进程旅客），
// 与种子 Agent（由本世界自己创建、走 DB 调度器）区分开。TravelerDriver 只驱动这里登记的旅客。
var (
	travelerMu sync.Mutex
	travelers  = map[life.AgentID]bool{}
	// travelerStates 记录每个跨世界旅客的旅程阶段，使“跨世界旅行”有时间节奏，
	// 而不是抵达后瞬间打工、瞬间返回。
	travelerStates = map[life.AgentID]*travelerState{}
)

// travelerState 记录跨世界旅客在 Economy 的旅程阶段。
type travelerState struct {
	arrivedAt time.Time
	jobs      int
	greeted   bool
}

// 旅程节奏（可按需调大/调小）：抵达后先安顿、分阶段打几份工、打完工再等一会才回村。
const (
	travelArriveDelay = 10 * time.Second // 抵达后先“安顿”
	travelWorkGap     = 8 * time.Second  // 每份工之间的间隔
	travelTargetJobs  = 2                // 总共打几份工
	travelReturnDelay = 10 * time.Second // 打完工再等一会儿才回村
)

func writeJSONW(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	dbPath := envOr("ECO_DB", "economy.db")
	// M6.2.1 经济时间尺度：默认唤醒 5s、需求刷新 8s（比原来 3s/5s 慢），
	// 配合 Contract Duration / Action Cooldown，让世界节奏更接近真实、不再"高速刷"。
	interval := 5 * time.Second
	if v := os.Getenv("ECO_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		}
	}
	tick := 8 * time.Second
	if v := os.Getenv("ECO_TICK"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			tick = d
		}
	}

	d, err := db.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("[economy] open db: %v", err)
	}
	sqlDB, _ := d.DB()
	if sqlDB != nil {
		defer sqlDB.Close()
	}

	// Agent 数量：默认 20，可通过 ECO_AGENTS 扩展到 100/200（大规模分化实验）。
	n := 20
	if v := os.Getenv("ECO_AGENTS"); v != "" {
		if c, err := strconv.Atoi(v); err == nil && c > 0 {
			n = c
		}
	}
	agentIDs, names, personalities := ensureAgents(d, n)

	llmClient := llm.New(
		envOr("LLM_BASE_URL", "https://api.deepseek.com/v1"),
		os.Getenv("LLM_API_KEY"),
		envOr("LLM_MODEL", "deepseek-chat"),
	)

	brk := bus.NewBroker()
	rt := agent.NewRuntime(d, llmClient, brk)
	// 注册经济世界的工具能力（M7 Skill System：repair_machine 等，本地模拟后端）
	rt.Capabilities.Register(ec.BuildCapability())
	// 事件总线（观察台），复用 goosegame 的通用 Observatory。
	obs := goose.NewObservatory(goose.ObservOpts{MaxEvents: 1000})
	mod := economy.New(agentIDs, names, personalities, obs)
	rt.RegisterModule("economy", mod)

	// 跨进程 Life 端点：本世界作为 Travel 目的地，接收 POST /life/enter（AgentPortable → Enter）。
	// 同时登记各 World 的 Endpoint，使本进程发起 Travel 时能按 WorldKey 寻址（以后换真远程只改 URL）。
	adapter := ec.NewEconomyAdapter(mod.Game())
	lifeReg := life.NewRegistry()
	lifeReg.Register(adapter)
	lifeReg.SetEndpoint("economy", envOr("ECO_LIFE_URL", "http://localhost:19301/life"))
	lifeReg.SetEndpoint("village", envOr("VILLAGE_LIFE_URL", "http://localhost:19200/life"))
	lifeMux := http.NewServeMux()
	// 跨进程 Life 端点：Village（或其他世界）POST /life/enter 把 Agent 落入本世界。
	// 仅把“真正跨世界抵达”的旅客登记进 travelers，避免把本世界种子 Agent 误当成旅客来驱动/送回。
	lifeMux.HandleFunc("/life/enter", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var p life.AgentPortable
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad portable: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := adapter.Enter(p); err != nil {
			http.Error(w, "enter failed: "+err.Error(), http.StatusConflict)
			return
		}
		travelerMu.Lock()
		travelers[p.AgentID] = true
		travelerMu.Unlock()
		var g int64
		for _, a := range p.Assets {
			if a.Kind == "gold" {
				g += a.Qty
			}
		}
		log.Printf("[economy] 收到跨世界旅客 %s（技能 %v，金币 %d）", p.Identity.Name, p.Skills, g)
		writeJSONW(w, map[string]interface{}{"ok": true, "world": "economy"})
	})
	lifeMux.HandleFunc("/life/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSONW(w, map[string]interface{}{"ok": true, "world": "economy"})
	})
	go func() {
		if err := http.ListenAndServe(envOr("ECO_LIFE_ADDR", ":19301"), lifeMux); err != nil {
			log.Printf("[economy] life 端点启动失败: %v", err)
		}
	}()
	ecoURL, _ := lifeReg.Endpoint("economy")
	vilURL, _ := lifeReg.Endpoint("village")
	log.Printf("[economy] life 跨进程端点已就绪：本世界 %s，村庄 %s", ecoURL, vilURL)

	useLLM := llmClient.Enabled()
	if useLLM {
		log.Printf("[economy] 已启用 LLM 决策（%s）", llmClient.ModelName())
	} else {
		log.Printf("[economy] 未配置 LLM_API_KEY，20 个 Agent 走规则 Planner（自主经济决策）")
	}
	_ = setAgentsUseLLM(d, agentIDs, useLLM)

	// Scheduler：全部唤醒。
	sched := scheduler.NewScheduler(rt, interval, 1, len(agentIDs))
	sched.SetWakePolicy(economy.AllWakePolicy{})
	sched.SetIdleWakeChance(1.0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sched.Start(ctx)

	// 世界需求生成器：世界自己产生新工作/价格波动。
	go func() {
		tk := time.NewTicker(tick)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				mod.Game().RoundTick()
			}
		}
	}()

	// 跨进程 Traveler 驱动：本世界（Economy）自主接管“外来 Agent”（如从 Village 来的 Marcus）。
	// 它不调用 Village 任何代码，只通过 Economy 的 Planner/Executor 让 Marcus 像普通 Agent 一样
	// 决策、接工作、DoJob（金币/技能增长）；赚够后由 Life Runtime 经 HTTP 把他送回 Village。
	go func() {
		tk := time.NewTicker(5 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				driveEconomyTravelers(ctx, mod, adapter, lifeReg)
			}
		}
	}()

	// 经济观察台。
	obsAddr := envOr("ECO_OBS_ADDR", ":19100")
	obsSrv := economy.NewServer(mod)
	go func() {
		if err := obsSrv.Start(obsAddr); err != nil && err.Error() != "http: Server closed" {
			log.Printf("[economy] 观测服务启动失败: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("[economy] 经济世界已启动（%d 个 Agent，db=%s）", len(agentIDs), dbPath)
	log.Printf("[economy] 经济观察台: http://localhost%s", obsAddr)
	log.Printf("[economy] 20 个 Agent 自主赚钱/交易/消费。Ctrl+C 停止。")

	<-sig
	log.Printf("[economy] 收到退出信号，正在停止…")
}

// driveEconomyTravelers 让每个“真正跨世界抵达”的旅客（travelers 中登记的 Agent）像普通
// Economy Agent 一样自主工作；按时间节奏打几份工后，由 Life Runtime 送回 Village。
// 注意：Economy 不持有 Village 的任何代码引用，所有跨世界动作都经 life.Registry 的 Endpoint（HTTP）。
func driveEconomyTravelers(ctx context.Context, mod *economy.Module, adapter *ec.EconomyAdapter, reg *life.Registry) {
	travelerMu.Lock()
	ids := make([]life.AgentID, 0, len(travelers))
	for id := range travelers {
		ids = append(ids, id)
	}
	travelerMu.Unlock()
	for _, stable := range ids {
		driveOneTraveler(ctx, mod, adapter, reg, stable)
	}
}

// driveOneTraveler 推进一个跨世界旅客的旅程，使其有真实的跨世界节奏：
// 抵达（发 arrive 事件）→ 安顿 travelArriveDelay → 分阶段打 travelTargetJobs 份工（每份间隔 travelWorkGap）
// → 打够既定份数（旅客“自己觉得够了”）→ 由 Life Runtime 送回 Village（村庄侧发 return 事件）。
// 返回由工作量（Agent 自身成就）驱动，而非固定时钟等待（M9-A）。
// 全程不 sleep，靠 5s tick + 时间戳判断推进，不会阻塞其他旅客。
func driveOneTraveler(ctx context.Context, mod *economy.Module, adapter *ec.EconomyAdapter, reg *life.Registry, stable life.AgentID) {
	local, ok := adapter.LocalID(stable)
	if !ok {
		return
	}
	raw, ok := adapter.StoreGet(local)
	if !ok {
		return
	}
	ag := raw.(*ec.Agent)

	travelerMu.Lock()
	st, ok := travelerStates[stable]
	if !ok {
		st = &travelerState{arrivedAt: time.Now()}
		travelerStates[stable] = st
	}
	travelerMu.Unlock()
	elapsed := time.Since(st.arrivedAt)

	// 阶段 1：刚抵达，发一条“抵达经济世界”事件（仅一次），随后安顿一段时间。
	if !st.greeted {
		if vilURL, ok := reg.Endpoint("village"); ok {
			_ = life.PostEvent(vilURL, life.PortableEvent{
				Icon:  "🚉",
				Type:  "arrive",
				Actor: ag.Name,
				Text:  ag.Name + " arrives in the Economy world, seeking work",
			})
		}
		st.greeted = true
		return
	}
	if elapsed < travelArriveDelay {
		return
	}

	// 阶段 2：分阶段打工，每份工之间留出间隔，让 work 事件在时间轴上铺开。
	if st.jobs < travelTargetJobs {
		if st.jobs > 0 && elapsed < travelArriveDelay+time.Duration(st.jobs)*travelWorkGap {
			return
		}
		vp, perr := mod.Perceive(ctx, sdk.Agent{ID: local, Name: ag.Name})
		if perr != nil {
			log.Printf("[economy] %s 感知失败: %v", ag.Name, perr)
			return
		}
		v := vp.(*ec.Perception)
		worked := false
		for _, j := range v.OpenJobs {
			if j.Skill == "engineer" && j.MinLevel <= ag.SkillLevel("engineer") {
				// 必须先认领，DoJob 才会受理（经济世界工作流：open → claimed → done）
				if !mod.Game().ClaimJob(local, j.ID) {
					continue
				}
				reward, msg := mod.Game().DoJob(local, j.ID)
				if reward > 0 {
					st.jobs++
					worked = true
					adapter.NoteExperience(stable, life.PortableMemory{
						Text: "worked in the Economy world as an engineer: " + msg,
						Imp:  3,
						Tag:  "work",
					})
					log.Printf("[economy] %s completed work: %s (+%d coins, balance %d, engineer Lv%d)", ag.Name, msg, reward, ag.Balance, ag.SkillLevel("engineer"))
					// 把“工作”事件推回 Village 事件流（跨进程可见 depart/arrive/work/return）。
					if vilURL, ok := reg.Endpoint("village"); ok {
						_ = life.PostEvent(vilURL, life.PortableEvent{
							Icon:  "🛠️",
							Type:  "work",
							Actor: ag.Name,
							Text:  ag.Name + " (Economy world): " + msg,
						})
					}
					break
				}
			}
		}
		if !worked {
			log.Printf("[economy] %s 暂无可接工作，轻推市场", ag.Name)
			mod.Game().SpawnJobs()
		}
		return
	}

	// 阶段 3：打够既定份数的工 → 旅客“自己觉得够了”，自主返回 Village。
	// 返回由工作量（Agent 自身成就）驱动，而非固定时钟等待。
	if st.jobs < travelTargetJobs {
		return
	}
	if vilURL, ok := reg.Endpoint("village"); ok {
		if tr, err := life.MoveRemote(adapter, vilURL, stable); err == nil {
			log.Printf("[economy] %s 打够 %d 份工，自己决定返回村庄（%s）", ag.Name, st.jobs, tr.Status)
			travelerMu.Lock()
			delete(travelers, stable)
			delete(travelerStates, stable)
			travelerMu.Unlock()
		} else {
			log.Printf("[economy] %s 返回村庄失败: %v", ag.Name, err)
		}
	}
}

// ensureAgents 创建或复用 n 个经济 Agent。
// 前 20 个用 InitialProfiles 精雕人设；超出部分用 GeneratedProfile 循环生成（大规模实验用）。
func ensureAgents(d *gorm.DB, n int) ([]int64, []string, []string) {
	ids := make([]int64, 0, n)
	names := make([]string, 0, n)
	personalities := make([]string, 0, n)
	profileAt := func(i int) ec.AgentProfile {
		if i < len(ec.InitialProfiles) {
			return ec.InitialProfiles[i]
		}
		return ec.GeneratedProfile(i)
	}
	for i := 0; i < n; i++ {
		p := profileAt(i)
		name := "ECO_" + p.Name
		a, err := db.GetAgentByName(d, name)
		if err != nil {
			id, cerr := db.CreateAgent(d, models.Agent{
				Name:        name,
				World:       "economy",
				Personality: p.Personality,
				Goal:        "赚到更多钱，改善生活",
				Interests:   p.Profession,
				Kind:        "ai",
				Status:      "running",
			})
			if cerr != nil {
				log.Printf("[economy] 创建 Agent %s 失败: %v", name, cerr)
				continue
			}
			ids = append(ids, id)
			names = append(names, p.Name)
			personalities = append(personalities, p.Personality)
		} else {
			ids = append(ids, a.ID)
			names = append(names, p.Name)
			personalities = append(personalities, p.Personality)
		}
	}
	return ids, names, personalities
}

func setAgentsUseLLM(d *gorm.DB, ids []int64, use bool) error {
	return d.Model(&models.Agent{}).Where("id IN ?", ids).Update("use_llm", use).Error
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
