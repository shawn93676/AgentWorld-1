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
	p := life.AgentPortable{
		AgentID: ag.Name,
		Identity: life.PortableIdentity{
			ID:         ag.Name,
			Name:       ag.Name,
			Occupation: ag.Profession,
		},
		Personality: life.PortablePersonality{Traits: splitTraits(ag.Personality), Goal: ag.Goal},
		Life:        life.PortableLife{State: a.BaseAdapter.StateOf(ag.ID), Energy: 100, Mood: 0},
	}
	for _, s := range ag.Skills {
		p.Skills = append(p.Skills, life.PortableSkill{Name: s.SkillID, Level: s.Level})
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
		skills = append(skills, skill.AgentSkill{SkillID: s.Name, Level: s.Level})
	}
	balance := int64(0)
	for _, asset := range p.Assets {
		if asset.Kind == "gold" {
			balance += asset.Qty
		}
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
