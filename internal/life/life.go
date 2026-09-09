// Package life 提供 Life Runtime 的“窄层”能力（M9 中现有 internal/agent + sdk +
// scheduler 框架尚未覆盖的部分）：Agent 生命状态枚举（LifeState）、跨 World 携带契约
// （AgentPortable）、每世界桥接（PortableAdapter），以及跨 World 移动事务（Move）。
//
// 设计边界（与现有框架正交，不重复实现）：
//   - internal/agent 的 Runtime 通过 sdk.Module 驱动所有世界（Perceive→Planner→Executor），
//     scheduler.Scheduler 按 WakePolicy 唤醒——这套已是 Agent 宿主框架；
//   - life 只补三件事：LifeState 生命周期语义、AgentPortable 携带契约、Move 事务；
//   - 每个世界实现自己的 PortableAdapter（Export/Import/CanAccept），把 AgentPortable
//     翻译成“本世界语义”，绝不把 A 世界的规则泄漏进 B 世界。
//
// 给世界开发者的 ergonomics（让接一个新世界只需写翻译，不写样板）：
//   - BaseAdapter：可嵌入，免费提供 id 映射 / LifeState 代管 / Leave·Enter·Export·LocalID·SelfTest；
//     世界只需实现 PortableCore 钩子（seedNames / store* / exportLocked / importLocked）。
//   - Registry：adapter 自注册，Move 可按 WorldKey 寻址，无需在 harness 中手动传递实例。
//   - SelfTest：导出→导入→再导出的金标准往返校验，每个新世界白送正确性检查。
package life

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNoTransporter Move 的 from/to 任一为 nil。
var ErrNoTransporter = errors.New("life: nil transporter")

// AgentID 全局稳定身份（跨 World 不变）。仅作语义标记；具体承载由 internal/agent 的
// models.Agent.World 字段完成（Agent 已可跨 World 存在）。
type AgentID = string

// StableID 构造全局稳定的 Agent 身份：<worldKey>:<local>。
// 用“起源世界 + 本地 ID”而非展示名，避免两个世界各有同名 Agent（如都叫 "Marcus"）
// 时在本世界 Enter 时被互相覆盖。展示名仍走 AgentPortable.Identity.Name。
// 注意：local 取“该 Agent 在起源世界的本地 ID”，因此同一 Agent 离场再入场时
// 稳定 ID 保持不变，跨世界往返可正确还原。
func StableID(worldKey string, local int64) AgentID {
	return AgentID(worldKey + ":" + strconv.FormatInt(local, 10))
}

// ParseStableID 反向解析 StableID，返回起源世界与本地 ID。
func ParseStableID(id AgentID) (worldKey string, local int64, err error) {
	parts := strings.SplitN(string(id), ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("life: invalid stable id %q", id)
	}
	local, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("life: invalid stable id %q: %w", id, err)
	}
	return parts[0], local, nil
}

// LifeState Agent 的生命周期状态（M9 §lifecycle）。现有框架只有 models.Agent.Status
// ("running")，没有休眠/旅行/死亡/归档语义；这是本包补充的核心类型。
type LifeState string

const (
	LifeAlive     LifeState = "alive"     // 在世，可被唤醒并决策
	LifeSleeping  LifeState = "sleeping"  // 休眠，不参与决策
	LifeTraveling LifeState = "traveling" // 跨 World 移动事务进行中
	LifeDead      LifeState = "dead"      // 死亡，不再唤醒
	LifeArchived  LifeState = "archived"  // 归档，状态冻结
)

// IsActive 是否处于可被框架唤醒 / 参与模拟的状态。
// sleeping/traveling/dead/archived 都不算 active（Think 循环应跳过）。
func (s LifeState) IsActive() bool { return s == LifeAlive }

// ---- AgentPortable：跨 World 携带契约（纯数据，不描述目标世界如何解释） ----

// AgentPortable 是 Agent “带着什么”跨 World 的纯数据表达。
// 它刻意不规定目标世界怎么解释这些字段——那只由 PortableAdapter 决定。
// 例：Skill{Blacksmith,Lv8} 在 Village 是“铁匠”，在 Economy 是“生产技能”，
// 在 Goose/Pascal 可能暂时无意义——这恰好是 World 独立性的证明，无需统一。
type AgentPortable struct {
	AgentID       AgentID
	Identity      PortableIdentity
	Personality   PortablePersonality
	Skills        []PortableSkill
	Relationships []PortableRelationship
	Memories      []PortableMemory
	Assets        []PortableAsset // 第一版可能为空（Gold/Inventory 等暂不支持跨 World）
	Life          PortableLife
}

// PortableIdentity 身份基元。
type PortableIdentity struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Occupation string `json:"occupation,omitempty"`
	Emoji      string `json:"emoji,omitempty"`
}

// PortablePersonality 人格与目标。
type PortablePersonality struct {
	Traits []string `json:"traits,omitempty"`
	Goal   string   `json:"goal,omitempty"`
}

// PortableSkill 一项技能。
type PortableSkill struct {
	Name  string `json:"name"`
	Level int    `json:"level"` // 0..N
}

// PortableRelationship 一条单向关系（以稳定 AgentID 为 key）。
type PortableRelationship struct {
	TargetID string `json:"target_id"`
	Like     int    `json:"like"`  // 0..100
	Trust    int    `json:"trust"` // 0..100
}

// PortableMemory 一条记忆。Src/Tag/EchoDay/Echoed 服务于长程因果（玩家干预的回响）。
type PortableMemory struct {
	Day     int    `json:"day"`
	Minute  int    `json:"minute"`
	Text    string `json:"text"`
	Imp     int    `json:"imp"`                // 1~5
	Src     string `json:"src,omitempty"`      // "player" = 玩家留下的痕迹
	Tag     string `json:"tag,omitempty"`      // 干预类型
	EchoDay int    `json:"echo_day,omitempty"` // 计划回响日
	Echoed  bool   `json:"echoed,omitempty"`
}

// PortableAsset 一项资产（第一版可能不被目标世界接受）。
type PortableAsset struct {
	Kind string `json:"kind"` // "gold" | "item" | ...
	Name string `json:"name"`
	Qty  int64  `json:"qty"`
}

// PortableLife 生命周期与即时状态（跨 World 携带）。
type PortableLife struct {
	State  LifeState `json:"state"`
	Energy int       `json:"energy,omitempty"` // 0..100
	Mood   int       `json:"mood,omitempty"`   // -100..100
}

// ---- Move 事务所需的极简接口（直接用 AgentPortable 作为携带体） ----

// Transporter 一个 World 向 Life Runtime 暴露的最小“进出”能力，比 sdk.Module 更窄：
// 只管携带状态的进出，不管感知 / 决策 / 执行（那些由 sdk.Module 负责）。
// PortableAdapter 同时满足此接口。
type Transporter interface {
	Leave(id AgentID) (AgentPortable, error) // 取出并冻结该 Agent 的便携状态
	Enter(p AgentPortable) error             // 接收并解冻
}

// keyer 可选接口：Transporter/Adapter 若实现 Key()，其返回值用作 Transition 的 From/To。
type keyer interface{ Key() string }

func worldKey(t Transporter) string {
	if k, ok := t.(keyer); ok {
		return k.Key()
	}
	return "?"
}

// ---- PortableAdapter：每世界的桥（把 AgentPortable 翻译成“本世界语义”） ----

// PortableAdapter 由每个世界实现，是 AgentPortable 与本世界 Agent 之间的双向翻译器。
//   - Export：本世界 Agent → AgentPortable（“带着什么走”）
//   - Import：AgentPortable → 本世界 Agent（“怎么落地”）
//   - CanAccept：目标世界是否能接纳（如 Economy 可能要求至少有一项可用技能）
// 它同时实现 Transporter（Leave/Enter），因此可直接传给 Move。
//
// 注意：Export 的入参是 any（边界处失去类型安全，由各世界内部断言回自己的 *Agent），
// 这是“单一共享契约”与“各世界自有 Agent 类型”之间的唯一让步点。
//
// 推荐做法：世界 adapter 嵌入 *BaseAdapter 并实现 PortableCore，即可免费获得本接口的全部方法。
type PortableAdapter interface {
	Transporter
	WorldKey() string
	CanAccept(p AgentPortable) bool
	Import(p AgentPortable) error
	Export(a any) (AgentPortable, error)
}

// Transition 跨 World 移动的事务记录（M9 §19）：防复制 / 防丢失。
type Transition struct {
	AgentID AgentID
	From    string
	To      string
	Status  TransitionStatus
	At      time.Time
	Err     string
}

// TransitionStatus 事务状态。
type TransitionStatus string

const (
	TransitionPending   TransitionStatus = "pending"
	TransitionCompleted TransitionStatus = "completed"
	TransitionFailed    TransitionStatus = "failed"
)

// Move 在两个 World 间事务性地转移 Agent（M9 §19）。
// 步骤：pending → from.Leave → to.Enter → completed；任一步失败则尝试回滚
// （重新 from.Enter）并标记 failed。
// 保证 Agent 不会在流转中被复制出两份，也不会凭空消失。
func Move(from, to Transporter, id AgentID) (Transition, error) {
	t := Transition{AgentID: id, From: worldKey(from), To: worldKey(to),
		Status: TransitionPending, At: time.Now()}
	if from == nil || to == nil {
		t.Status = TransitionFailed
		t.Err = ErrNoTransporter.Error()
		return t, ErrNoTransporter
	}
	carried, err := from.Leave(id)
	if err != nil {
		t.Status = TransitionFailed
		t.Err = fmt.Sprintf("leave %s: %v", t.From, err)
		return t, err
	}
	if err := to.Enter(carried); err != nil {
		_ = from.Enter(carried) // 回滚：尝试重新接纳
		t.Status = TransitionFailed
		t.Err = fmt.Sprintf("enter %s: %v", t.To, err)
		return t, err
	}
	t.Status = TransitionCompleted
	return t, nil
}

// ---- PortableCore：世界开发者需实现的钩子（BaseAdapter 通用逻辑调用它） ----

// PortableCore 由每个世界实现，封装“本世界与 Agent 存储相关的世界特定操作”。
// BaseAdapter 调用这些钩子完成通用的 Leave/Enter/LocalID/SelfTest，从而让世界开发者
// 只需写翻译（ExportLocked/ImportLocked）与存储钩子，不必重复 id 映射 / 锁 / 事务样板。
//
// 钩子方法均为导出（Go 要求跨包实现接口时方法必须导出）。它们由 BaseAdapter 在持锁状态下
// 调用：所有钩子在调用时已持有 BaseAdapter 的互斥锁（b.mu），因此可直接读 b.revMap /
// b.state（通过 NameOf / StateOf，二者无锁、要求调用方持锁）。世界自身的存储锁（如 w.mu）
// 由钩子内部自行加解锁；保持“b.mu 总是先于 w.mu 获取”即可避免死锁。
type PortableCore interface {
	WorldKey() string
	CanAccept(p AgentPortable) bool

	// SeedNames 返回初始 稳定名->本地ID 映射（用于本世界已存在的 Agent）。
	SeedNames() map[string]int64
	// StoreGet / StorePut / StoreRemove 是本世界 Agent 存储的查/增改/删。
	StoreGet(local int64) (any, bool)
	StorePut(local int64, ag any)
	StoreRemove(local int64)
	// StoreAllocLocal 为新入境 Agent 分配一个尚未使用的本地 ID。
	StoreAllocLocal() int64

	// ExportLocked 把本世界 Agent 翻译为 AgentPortable（纯翻译；local 用于关系反向映射）。
	// 调用时已持 b.mu，可通过 NameOf / StateOf 读取映射与状态。
	ExportLocked(local int64, ag any) (AgentPortable, error)
	// ImportLocked 把 AgentPortable 翻译为本世界 Agent（可能带副作用，如把无法表达
	// 的字段存入 carry 侧边 map；SelfTest 会通过可选的 cleanup 钩子撤销这些副作用）。
	ImportLocked(local int64, p AgentPortable) (any, error)
}

// cleaner 可选钩子：SelfTest 在试导入后调用，用于撤销 importLocked 的副作用
// （例如清空 carry 侧边 map），避免污染真实 adapter。
type cleaner interface{ cleanup(local int64) }

// ---- BaseAdapter：通用骨架，世界 adapter 嵌入即可免费获得大部分能力 ----

// BaseAdapter 提供 PortableAdapter 的通用实现骨架。世界开发者让自己的 adapter 嵌入
// *BaseAdapter、在构造时调用 Init(a)，并实现 PortableCore 钩子，即可免费获得
// id 映射、LifeState 代管、Leave/Enter/Export/LocalID/SelfTest。
//
// 示例：
//
//	type FooAdapter struct {
//		*life.BaseAdapter
//		w *world.Foo
//	}
//	func NewFooAdapter(w *world.Foo) *FooAdapter {
//		a := &FooAdapter{w: w}
//		a.BaseAdapter = &life.BaseAdapter{}
//		a.BaseAdapter.Init(a)
//		return a
//	}
//	// 然后实现 PortableCore 的 WorldKey/CanAccept/seedNames/store*/exportLocked/importLocked。
type BaseAdapter struct {
	mu   sync.Mutex
	core PortableCore

	idMap  map[string]int64
	revMap map[int64]string
	idRev  map[int64]AgentID // local -> 全局稳定身份（跨世界携带不变）
	state  map[int64]LifeState
}

// Init 由世界 adapter 在构造时调用，完成快照与映射初始化。core 即世界 adapter 自身。
func (b *BaseAdapter) Init(core PortableCore) {
	b.core = core
	b.idMap = map[string]int64{}
	b.revMap = map[int64]string{}
	b.idRev = map[int64]AgentID{}
	b.state = map[int64]LifeState{}
	for name, local := range core.SeedNames() {
		stable := StableID(core.WorldKey(), local)
		b.idMap[string(stable)] = local
		b.revMap[local] = name
		b.idRev[local] = stable
		b.state[local] = LifeAlive
	}
}

// StateOf 返回本地 Agent 的生命状态。调用方必须已持有 b.mu。
func (b *BaseAdapter) StateOf(local int64) LifeState { return b.state[local] }

// NameOf 返回本地 ID 对应的展示名。调用方必须已持有 b.mu。
func (b *BaseAdapter) NameOf(local int64) string { return b.revMap[local] }

// StableIDOf 返回本地 Agent 的全局稳定身份（跨世界携带不变，由 Seed/Enter 设置）。
// 调用方必须已持有 b.mu。adapter 在 ExportLocked 中应优先用它，而不是按“当前世界 local”
// 重算——否则 Agent 每进入一个新世界，其稳定身份就会变，跨世界往返无法复用原点槽位。
func (b *BaseAdapter) StableIDOf(local int64) AgentID { return b.idRev[local] }

// LocalID 返回某稳定 ID 在本世界的本地 int64（供 harness / 外部定位 Agent）。
func (b *BaseAdapter) LocalID(pid AgentID) (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.idMap[string(pid)]
	return id, ok
}

// LocalIDUnlocked 同 LocalID 但不持锁；调用方必须已持有 b.mu。用于本世界 import/export
// 钩子内部（这些钩子总是被 BaseAdapter 在持锁状态下调用）。
func (b *BaseAdapter) LocalIDUnlocked(pid AgentID) (int64, bool) {
	id, ok := b.idMap[string(pid)]
	return id, ok
}

// TravelerIDs 返回当前在场（通过 Enter 进入本世界）的 Agent 稳定身份。
// 种子 Agent 不走 adapter.Enter，不会列入——只有“外来旅客”会。Travel 驱动据此识别需要被
// 本世界自主接管的跨世界 Agent（例如 Economy 里的 Marcus）。
func (b *BaseAdapter) TravelerIDs() []AgentID {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]AgentID, 0, len(b.idMap))
	for id := range b.idMap {
		ids = append(ids, AgentID(id))
	}
	return ids
}

// WorldKey 委派给 core。
func (b *BaseAdapter) WorldKey() string { return b.core.WorldKey() }

// CanAccept 委派给 core。
func (b *BaseAdapter) CanAccept(p AgentPortable) bool { return b.core.CanAccept(p) }

// Leave 取出并冻结 Agent（从世界中移除），返回便携状态。
func (b *BaseAdapter) Leave(id AgentID) (AgentPortable, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	local, ok := b.idMap[string(id)]
	if !ok {
		return AgentPortable{}, fmt.Errorf("%s: agent %q not found", b.core.WorldKey(), id)
	}
	ag, ok := b.core.StoreGet(local)
	if !ok {
		return AgentPortable{}, fmt.Errorf("%s: agent %q missing", b.core.WorldKey(), id)
	}
	p, err := b.core.ExportLocked(local, ag)
	if err != nil {
		return AgentPortable{}, err
	}
	b.core.StoreRemove(local)
	b.state[local] = LifeTraveling
	return p, nil
}

// Enter 接收便携状态，在本世界重建 Agent（冻结过的 Agent 复用原本地 ID，关系可正确还原）。
func (b *BaseAdapter) Enter(p AgentPortable) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	local, ok := b.idMap[p.AgentID]
	if !ok {
		local = b.core.StoreAllocLocal()
		b.idMap[p.AgentID] = local
	}
	b.revMap[local] = p.Identity.Name // 展示名（本地回查用）
	b.idRev[local] = p.AgentID         // 稳定身份（跨世界携带不变）
	ag, err := b.core.ImportLocked(local, p)
	if err != nil {
		return err
	}
	b.core.StorePut(local, ag)
	st := p.Life.State
	if st == "" {
		st = LifeAlive
	}
	b.state[local] = st
	return nil
}

// Import 是 Enter 的别名（满足 PortableAdapter 接口；二者语义相同：把便携状态落入本世界）。
func (b *BaseAdapter) Import(p AgentPortable) error { return b.Enter(p) }

// Export 把任意本世界 Agent（any）翻译为 AgentPortable（纯快照，不修改世界、不进出）。
// 注意：直接 Export 时无法反查本地 ID（仅用于快照）；完整进出请用 Leave/Enter。
func (b *BaseAdapter) Export(a any) (AgentPortable, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.core.ExportLocked(0, a)
}

// SelfTest 对给定 AgentPortable 做一次 导入→导出 的往返校验：若两次导出逐项相等，
// 说明该世界的 adapter 翻译是幂等可还原的。这是每个新世界接入时可白送的正确性检查。
// 例：vadapter.SelfTest(samplePortable) 返回 nil 即表示该 adapter 往返一致。
func (b *BaseAdapter) SelfTest(p AgentPortable) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	prevLocal, had := b.idMap[p.AgentID] // 若该 Agent 已在本世界（如 seed 进来的 Marcus），试后须还原映射
	local := b.core.StoreAllocLocal()
	b.idMap[p.AgentID] = local
	b.revMap[local] = p.AgentID
	b.idRev[local] = p.AgentID
	st := p.Life.State
	if st == "" {
		st = LifeAlive
	}
	b.state[local] = st // 与 Enter 一致，使导出时的 LifeState 可还原
	defer func() {
		if had {
			b.idMap[p.AgentID] = prevLocal // 还原既有映射，避免 SelfTest 污染真实 idMap
		} else {
			delete(b.idMap, p.AgentID)
		}
		delete(b.revMap, local)
		delete(b.idRev, local)
		delete(b.state, local)
		if c, ok := b.core.(cleaner); ok {
			c.cleanup(local)
		}
	}()
	ag, err := b.core.ImportLocked(local, p)
	if err != nil {
		return fmt.Errorf("%s: SelfTest import: %w", b.core.WorldKey(), err)
	}
	out, err := b.core.ExportLocked(local, ag)
	if err != nil {
		return fmt.Errorf("%s: SelfTest export: %w", b.core.WorldKey(), err)
	}
	out.AgentID = p.AgentID // 归一化稳定 ID 后再比较
	out.Identity.ID = p.Identity.ID // adapter 可能把 ID 归一化为稳定身份，同样归一化
	if !portableEqual(p, out) {
		return fmt.Errorf("%s: SelfTest round-trip mismatch\n  in : %+v\n  out: %+v", b.core.WorldKey(), p, out)
	}
	return nil
}

// portableEqual 逐项比较两份 AgentPortable（结构体字段、切片顺序敏感；无 map，JSON 稳定）。
func portableEqual(a, b AgentPortable) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(aa) == string(bb)
}

// ---- Registry：可选的世界注册中心，按 WorldKey 自注册与寻址 ----

// Registry 一个可选的 World 注册中心：各世界 adapter 自注册后，可按 WorldKey 寻址，
// 无需在 harness 中手动传递 adapter 实例。适合“真实多世界系统”而非一次性 demo。
type Registry struct {
	mu       sync.Mutex
	adapters map[string]PortableAdapter
	endpoints map[string]string // WorldKey → 该世界 Life API 的 HTTP 基址（如 http://host:port/life）
}

// NewRegistry 构造一个空注册中心。
func NewRegistry() *Registry {
	return &Registry{adapters: map[string]PortableAdapter{}, endpoints: map[string]string{}}
}

// SetEndpoint 登记某 WorldKey 对应的 Life API 基址（跨进程 Travel 时用于 POST /enter）。
// 以后换成真正远程服务器，只需改这里传入的 URL，Agent 生命周期模型不变。
func (r *Registry) SetEndpoint(key, url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoints[key] = url
}

// Endpoint 返回某 WorldKey 对应的 Life API 基址。
func (r *Registry) Endpoint(key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.endpoints[key]
	return u, ok
}

// Register 注册一个世界 adapter（以其 WorldKey 为索引）。
func (r *Registry) Register(a PortableAdapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.WorldKey()] = a
}

// Get 按 WorldKey 取出已注册的 adapter。
func (r *Registry) Get(key string) (PortableAdapter, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.adapters[key]
	return a, ok
}

// MustGet 按 WorldKey 取出 adapter，未注册则 panic（适合启动期校验所有世界均已注册）。
func (r *Registry) MustGet(key string) PortableAdapter {
	a, ok := r.Get(key)
	if !ok {
		panic(fmt.Sprintf("life: no adapter registered for world %q", key))
	}
	return a
}

// MoveByKey 按 WorldKey 寻址后执行 Move（保留 from/to 的 key 信息）。
func (r *Registry) MoveByKey(fromKey, toKey, id string) (Transition, error) {
	from, ok1 := r.Get(fromKey)
	to, ok2 := r.Get(toKey)
	if !ok1 || !ok2 {
		t := Transition{AgentID: AgentID(id), From: fromKey, To: toKey,
			Status: TransitionFailed, At: time.Now(),
			Err: fmt.Sprintf("registry: missing world(s): from=%s(%v) to=%s(%v)", fromKey, ok1, toKey, ok2)}
		return t, fmt.Errorf("%s", t.Err)
	}
	return Move(from, to, AgentID(id))
}

// MoveByKeyRemote 按 WorldKey 寻址后执行跨进程 Move（WorldKey → Endpoint → HTTP World API）。
// from 必须是本进程内已注册的 adapter（源）；to 仅需知道其 Endpoint URL（目的地可为远程进程）。
func (r *Registry) MoveByKeyRemote(fromKey, toKey, id string) (Transition, error) {
	from, ok1 := r.Get(fromKey)
	toURL, ok2 := r.Endpoint(toKey)
	if !ok1 || !ok2 {
		t := Transition{AgentID: AgentID(id), From: fromKey, To: toKey,
			Status: TransitionFailed, At: time.Now(),
			Err: fmt.Sprintf("registry: missing world(s): from=%s(%v) to=%s(%v)", fromKey, ok1, toKey, ok2)}
		return t, fmt.Errorf("%s", t.Err)
	}
	src, ok := from.(remoteSource)
	if !ok {
		t := Transition{AgentID: AgentID(id), From: fromKey, To: toKey,
			Status: TransitionFailed, At: time.Now(),
			Err: fmt.Sprintf("registry: source %q adapter lacks remote capability", fromKey)}
		return t, fmt.Errorf("%s", t.Err)
	}
	return MoveRemote(src, toURL, AgentID(id))
}

// SelfTestAll 对注册中心内每个 adapter 跑一次给定 sample 的往返校验，返回所有错误。
// sample 通常取“本世界一个代表性 Agent 的 AgentPortable”。未实现 SelfTest 的 adapter 会被跳过。
//
// 注意：sample 会被同一个 AgentPortable 喂给所有已注册世界。若各世界的便携契约不同
// （例如 village 携带 Energy/Mood、economy 导出时硬编码 Energy=100/Mood=0），共享样本
// 必然在某些世界上往返不一致而失败。此时应改用各 adapter 独立的 SelfTest，传入符合
// 各自契约的样本（见 internal/life/README.md 第 4 节与 experiments/m9）。
func (r *Registry) SelfTestAll(sample AgentPortable) []error {
	r.mu.Lock()
	adapters := make([]PortableAdapter, 0, len(r.adapters))
	for _, a := range r.adapters {
		adapters = append(adapters, a)
	}
	r.mu.Unlock()
	var errs []error
	for _, a := range adapters {
		if st, ok := a.(interface{ SelfTest(AgentPortable) error }); ok {
			if err := st.SelfTest(sample); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}
