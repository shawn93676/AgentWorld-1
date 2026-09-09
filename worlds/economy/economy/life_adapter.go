package economy

import (
	"strings"
	"sync"

	"agentworld/internal/life"
	"agentworld/internal/skill"
)

// EconomyAdapter 把 economy.Agent 与 life.AgentPortable 互译。
//
// 通过嵌入 life.BaseAdapter 复用 id 映射 / LifeState 代管 / Leave·Enter·Export·LocalID·SelfTest，
// 本文件只需实现世界特定的翻译与存储钩子（life.PortableCore）。
//
// 关键设计（证明 World 独立性）：
//   - economy 的 Personality 是单个字符串、Skills 是 []skill.AgentSkill、Relationships 是
//     map[int64]float64（信任 -1~1）、且没有“记忆”字段——与 village 完全不同；
//   - 经济世界原生不能表达的部分（记忆 / 关系）通过 carry* 随 Agent 往返，保证 Marcus 离场时
//     仍能把这些“人生”带出去，回到 Village 后原样还原；
//   - 金币用 economy 原生的 Balance 表达，进出时直接映射为 PortableAsset(gold)，无缝带回村庄；
//   - 同一个 Marcus：在 Village 是“铁匠”，在 Economy 是“生产技能”——互不污染。
type EconomyAdapter struct {
	*life.BaseAdapter
	w *World

	// carry：经济世界原生不表达的字段，随 Agent 携带往返（不写入 economy.Agent）。
	carryMu  sync.Mutex
	carryMem map[int64][]life.PortableMemory
	carryRel map[int64][]life.PortableRelationship
}

// NewEconomyAdapter 构造适配器，并快照当前所有 Agent 的 名字<->本地ID 映射。
func NewEconomyAdapter(w *World) *EconomyAdapter {
	a := &EconomyAdapter{
		w:        w,
		carryMem: map[int64][]life.PortableMemory{},
		carryRel: map[int64][]life.PortableRelationship{},
	}
	a.BaseAdapter = &life.BaseAdapter{}
	a.BaseAdapter.Init(a)
	return a
}

// 技能桥：把村庄(village)技能名翻译为 economy 技能 ID，使外来 Agent 能在 Economy 工作。
//
// 关键原则（与 adapter 注释“互不污染”一致）：Economy 永远不知道 "Blacksmithing" /
// "Trading" 等村庄技能——映射只存在于本 adapter 的转换层。规范技能名（如 "Blacksmithing"）
// 始终保存在 AgentPortable 里往返，本世界内部只认 "engineer" / "trader" 等 economy 技能。
//
// 这里用纯函数式双向映射（而非快照式 carrySkills）是因为：Export 要从 economy Agent 的
// 【当前】技能反推规范技能，这样 Economy 工作带来的等级提升（Blacksmithing Lv5 → 经
// engineer Lv6）才能正确带回村庄。快照式方案会卡在导入时的旧等级（Lv5），无法通过 Test 2。
var villageToEconomySkill = map[string]string{
	"Blacksmithing": "engineer",
	"Trading":       "trader",
	"Mining":        "miner",
	"Farming":       "farmer",
	"Cooking":       "chef",
	"Healing":       "doctor",
	"Delivering":    "courier",
}

// economyToVillageSkill 是 villageToEconomySkill 的反向映射（由前者自动构造）。
var economyToVillageSkill = func() map[string]string {
	m := make(map[string]string, len(villageToEconomySkill))
	for k, v := range villageToEconomySkill {
		m[v] = k
	}
	return m
}()

// 等级尺度桥：村庄技能是 0~100 熟练度，Economy 技能是 Lv1~7。两者必须互转，否则 Economy 会把
// 村庄的 Lv72 当成 Lv72、IncomeMultiplier/SkillSuccessRate/UpgradeSkill 全部失衡。
//   - village→economy：仅当来源明显是 0~100（>7）时才折算成 1~7；已是 1~7 则原样（如单测）。
//   - economy→village：1~7 折算回 0~100 区间中部，便于村庄“熟练度”自然增长。
// 这样“Marcus 在 Village 是铁匠(Lv72)，在 Economy 是 engineer(Lv5)，工作后 Lv6，回村变 Lv83”
// 互不污染，且等级提升能正确跨世界带回。
func villageToEconLevel(v int) int {
	if v <= 0 {
		return 1
	}
	if v > 7 {
		return 1 + v/15 // 72→5, 87→6
	}
	return v
}

func econToVillageLevel(v int) int {
	if v <= 0 {
		return 1
	}
	if v > 7 {
		return v // 已是村庄尺度，原样
	}
	return (v-1)*15 + 8 // 5→68, 6→83
}

func (a *EconomyAdapter) WorldKey() string { return "economy" }

func (a *EconomyAdapter) CanAccept(p life.AgentPortable) bool { return p.Identity.Name != "" }

// ---- life.PortableCore 钩子 ----

func (a *EconomyAdapter) SeedNames() map[string]int64 {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	m := make(map[string]int64, len(a.w.Agents))
	for id, ag := range a.w.Agents {
		m[ag.Name] = id
	}
	return m
}

func (a *EconomyAdapter) StoreGet(local int64) (any, bool) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	ag, ok := a.w.Agents[local]
	return ag, ok
}

func (a *EconomyAdapter) StorePut(local int64, ag any) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	a.w.Agents[local] = ag.(*Agent)
}

func (a *EconomyAdapter) StoreRemove(local int64) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	delete(a.w.Agents, local)
}

func (a *EconomyAdapter) StoreAllocLocal() int64 {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	local := a.w.nextAgent
	a.w.nextAgent++
	return local
}

func (a *EconomyAdapter) ExportLocked(local int64, ag any) (life.AgentPortable, error) {
	a.carryMu.Lock()
	defer a.carryMu.Unlock()
	return a.exportAgent(local, ag.(*Agent)), nil
}

func (a *EconomyAdapter) ImportLocked(local int64, p life.AgentPortable) (any, error) {
	a.carryMu.Lock()
	defer a.carryMu.Unlock()
	return a.importAgent(local, p), nil
}

// cleanup 撤销 importLocked 的副作用（SelfTest 用，避免污染真实 carry）。
func (a *EconomyAdapter) cleanup(local int64) {
	a.carryMu.Lock()
	defer a.carryMu.Unlock()
	delete(a.carryMem, local)
	delete(a.carryRel, local)
}

// ---- 世界特定的纯翻译 ----

// exportAgent: economy.Agent + carry -> AgentPortable（调用时已持 b.mu 与 carryMu）。
func (a *EconomyAdapter) exportAgent(local int64, ag *Agent) life.AgentPortable {
	pid := a.BaseAdapter.StableIDOf(local) // 携带不变的全局稳定身份（非按当前世界 local 重算）
	p := life.AgentPortable{
		AgentID: pid,
		Identity: life.PortableIdentity{
			ID:         pid,
			Name:       ag.Name,
			Occupation: ag.Profession,
		},
		Personality: life.PortablePersonality{Traits: splitTraits(ag.Personality), Goal: ag.Goal},
		Life:        life.PortableLife{State: ag.Life, Energy: 100, Mood: 0},
	}
	for _, s := range ag.Skills {
		// 技能桥（反向）：economy 技能 ID → 村庄规范技能名，让升级后的等级正确带回村庄。
		name := s.SkillID
		if canonical, ok := economyToVillageSkill[s.SkillID]; ok {
			name = canonical
		}
		p.Skills = append(p.Skills, life.PortableSkill{Name: name, Level: econToVillageLevel(s.Level)})
	}
	for _, m := range a.carryMem[local] {
		p.Memories = append(p.Memories, m)
	}
	for _, r := range a.carryRel[local] {
		p.Relationships = append(p.Relationships, r)
	}
	if ag.Balance != 0 {
		p.Assets = append(p.Assets, life.PortableAsset{Kind: "gold", Name: "gold", Qty: ag.Balance})
	}
	return p
}

// importAgent: AgentPortable -> economy.Agent（记忆/关系存入 carry，金币映射为 Balance）。
// 调用时已持 b.mu 与 carryMu。
func (a *EconomyAdapter) importAgent(local int64, p life.AgentPortable) *Agent {
	skills := make([]skill.AgentSkill, 0, len(p.Skills))
	for _, s := range p.Skills {
		// 技能桥：村庄规范技能名 → economy 技能 ID（economy 内部只认 engineer 等）。
		id := s.Name
		if mapped, ok := villageToEconomySkill[s.Name]; ok {
			id = mapped
		}
		skills = append(skills, skill.AgentSkill{SkillID: id, Level: villageToEconLevel(s.Level)})
	}
	balance := int64(0)
	for _, asset := range p.Assets {
		if asset.Kind == "gold" {
			balance += asset.Qty
		}
	}
	st := p.Life.State
	if st == "" {
		st = life.LifeAlive
	}
	ag := &Agent{
		ID:            local,
		Name:          p.Identity.Name,
		Kind:          "ai",
		Profession:    p.Identity.Occupation,
		Personality:   joinTraits(p.Personality.Traits),
		Goal:          p.Personality.Goal,
		Balance:       balance,
		Inventory:     map[string]int{},
		Skills:        skills,
		Life:          st,
		Relationships: map[int64]float64{},
	}
	// 经济世界原生不能表达的部分：存入 carry，离场时随 AgentPortable 带出。
	a.carryMem[local] = nil
	for _, m := range p.Memories {
		a.carryMem[local] = append(a.carryMem[local], m)
	}
	a.carryRel[local] = nil
	for _, r := range p.Relationships {
		a.carryRel[local] = append(a.carryRel[local], r)
	}
	return ag
}

// NoteExperience 给某 Agent 追加一段“跨世界经历”记忆（经济世界原生无记忆字段，由 carry 携带）。
func (a *EconomyAdapter) NoteExperience(pid life.AgentID, mem life.PortableMemory) {
	local, ok := a.BaseAdapter.LocalID(pid)
	if !ok {
		return
	}
	a.carryMu.Lock()
	a.carryMem[local] = append(a.carryMem[local], mem)
	a.carryMu.Unlock()
}

func splitTraits(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func joinTraits(ts []string) string {
	return strings.Join(ts, "，")
}
