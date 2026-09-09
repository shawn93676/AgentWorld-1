// hub.go —— 每游客一个独立世界（P0 底座）。
//
// 框架原生支持多世界共存（Runtime.RegisterModule(worldName, mod) + Scheduler 按
// Agent.World 分派），因此这里只需为每个游客（cookie uid）惰性构建一个完整的
// 世界实例：独立的 World / Module / Observatory / 世界时钟 / Scheduler，
// 共享底层 DB、LLM 客户端与 Runtime。游客身份通过 cookie 传递，每人快照存
// village_worlds/<uid>.json，从而支持"回来看看"。
package village

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agentworld/internal/agent"
	"agentworld/internal/db"
	"agentworld/internal/life"
	"agentworld/internal/llm"
	"agentworld/internal/models"
	"agentworld/internal/scheduler"
	"agentworld/worlds/goosegame/goose"
	vv "agentworld/worlds/village/village"
	"gorm.io/gorm"
)

// Instance 是单个游客（uid）拥有的独立村庄世界。
type Instance struct {
	UID       string
	WorldName string
	World     *vv.World
	Mod       *Module
	Obs       *goose.Observatory
	Adapter   *vv.VillageAdapter // Life Runtime 桥接（跨进程 Travel 的本地落地点）
	Snapshot  string
	dbIDs     []int64
}

// Hub 管理所有游客的世界，按需在首次访问时惰性创建并加载快照。
type Hub struct {
	mu      sync.Mutex
	m       map[string]*Instance
	factory func(uid string, ctx context.Context) (*Instance, error)
	ctx     context.Context
	primary *Instance
}

// NewHub 用工厂函数构建 Hub；工厂负责为一个 uid 创建完整世界实例。
func NewHub(factory func(uid string, ctx context.Context) (*Instance, error)) *Hub {
	return &Hub{m: map[string]*Instance{}, factory: factory}
}

// SetCtx 注入全局生命周期 context（用于各实例的时钟/调度 goroutine 退出）。
func (h *Hub) SetCtx(ctx context.Context) { h.ctx = ctx }

// Get 返回（或惰性创建）某游客的世界实例。调用期间持锁，保证每 uid 只创建一次。
func (h *Hub) Get(uid string) (*Instance, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if inst, ok := h.m[uid]; ok {
		return inst, nil
	}
	inst, err := h.factory(uid, h.ctx)
	if err != nil {
		return nil, err
	}
	h.m[uid] = inst
	if h.primary == nil && uid != "" {
		h.primary = inst
	}
	return inst, nil
}

// demoUID 返回演示世界 uid（通过环境变量 VILLAGE_DEMO_UID 指定）。
// 设置后，所有 web 访客与跨进程 Travel 都落在同一个世界里（含铁匠 Marcus），
// 便于展示“跨进程数字生命旅程”。未设置则保持原生多租户（每访客独立世界）。
func demoUID() string {
	if u := os.Getenv("VILLAGE_DEMO_UID"); u != "" {
		return u
	}
	return ""
}

// DefaultUID 返回本进程用作“演示/跨进程 Travel”的世界 uid：优先 VILLAGE_DEMO_UID，
// 否则退回到首个被创建的世界（primary）。
func (h *Hub) DefaultUID() string {
	if u := demoUID(); u != "" {
		return u
	}
	if h.primary != nil {
		return h.primary.UID
	}
	return ""
}

// PrimaryInstance 返回首个被创建的世界实例（多租户 fallback）。
func (h *Hub) PrimaryInstance() *Instance { return h.primary }

// LifeAdapter 返回某游客世界实例的 Life Runtime 桥（跨进程 Travel 的本地落地点）。
func (h *Hub) LifeAdapter(uid string) (life.PortableAdapter, error) {
	inst, err := h.Get(uid)
	if err != nil {
		return nil, err
	}
	return inst.Adapter, nil
}

// SaveAll 保存所有已创建的世界快照（退出或定时自动保存时调用）。
func (h *Hub) SaveAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for uid, inst := range h.m {
		if err := inst.World.Save(); err != nil {
			log.Printf("[village] 保存游客 %s 的世界失败: %v", uid, err)
		} else {
			log.Printf("[village] 已保存游客 %s 的世界", uid)
		}
	}
}

// NewInstance 为一个游客构建完整的独立世界：DB Agent + World + Module + 调度 + 世界时钟。
func NewInstance(uid string, ctx context.Context, d *gorm.DB, llmClient *llm.Client, rt *agent.Runtime, interval time.Duration, speed, dailyActs int, snapDir string) (*Instance, error) {
	worldName := "village_" + uid
	snapPath := filepath.Join(snapDir, uid+".json")
	ids, err := ensureAgentsFor(d, worldName, uid)
	if err != nil {
		return nil, err
	}
	obs := goose.NewObservatory(goose.ObservOpts{MaxEvents: 1000})
	world := vv.NewWorld(obs, snapPath) // NewWorld 内部会自动 Load 该 uid 的快照
	mod := New(world)
	rt.RegisterModule(worldName, mod) // 按 worldName 分派，支持多世界共存
	for i, id := range ids {
		world.Attach(id, vv.Profiles[i])
	}
	world.SealRelations()
	world.SetSpeed(speed)
	if dailyActs > 0 {
		world.SetDailyActs(dailyActs)
	}
	mod.EnableLLM(llmClient)

	inst := &Instance{UID: uid, WorldName: worldName, World: world, Mod: mod, Obs: obs, Snapshot: snapPath, dbIDs: ids}
	inst.Adapter = vv.NewVillageAdapter(world) // Life Runtime 桥：跨进程 Travel 在本世界落地

	// 世界时钟：每 tick 推进 speed 个游戏分钟
	go func() {
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				world.Tick()
			}
		}
	}()

	// 调度器：只唤醒本世界 Due 的 Agent（dueWakePolicy 按本世界 IsDue 判定）
	sched := scheduler.NewScheduler(rt, interval, 1, len(ids))
	sched.SetWakePolicy(mod.WakePolicy())
	sched.SetIdleWakeChance(1.0)
	go func() { sched.Start(ctx) }()

	log.Printf("[village] 游客 %s 的世界已苏醒（%d 村民，存档 %s）", uid, len(ids), snapPath)
	return inst, nil
}

// ensureAgentsFor 为该游客创建或复用 10 个村民的 DB 元数据（名字带 uid 前缀，
// World 字段指向本世界，使 Scheduler 能按 Agent.World 正确分派）。
func ensureAgentsFor(d *gorm.DB, worldName, uid string) ([]int64, error) {
	ids := make([]int64, 0, len(vv.Profiles))
	for _, p := range vv.Profiles {
		dbName := "VIL_" + uid + "_" + p.Name
		a, err := db.GetAgentByName(d, dbName)
		if err != nil {
			id, cerr := db.CreateAgent(d, models.Agent{
				Name:        dbName,
				World:       worldName,
				Personality: joinTraits(p.Personality),
				Goal:        p.Goal,
				Interests:   p.Occupation,
				Kind:        "ai",
				Status:      "running",
			})
			if cerr != nil {
				return nil, cerr
			}
			ids = append(ids, id)
		} else {
			ids = append(ids, a.ID)
			if a.Status != "running" {
				_ = db.SetAgentStatus(d, a.ID, "running")
			}
		}
	}
	if len(ids) != len(vv.Profiles) {
		return nil, fmt.Errorf("期望 %d 个村民，实际 %d 个", len(vv.Profiles), len(ids))
	}
	return ids, nil
}

func joinTraits(ts []string) string {
	out := ""
	for i, t := range ts {
		if i > 0 {
			out += ", "
		}
		out += t
	}
	return out
}

// randUID 生成游客身份（写入 cookie）。
func randUID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "anon"
	}
	return hex.EncodeToString(b[:])
}
