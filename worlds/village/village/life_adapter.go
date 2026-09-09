package village

import (
	"agentworld/internal/life"
)

// VillageAdapter 把 village.Agent 与 life.AgentPortable 互译。
//
// 通过嵌入 life.BaseAdapter 复用 id 映射 / LifeState 代管 / Leave·Enter·Export·LocalID·SelfTest，
// 本文件只需实现世界特定的翻译与存储钩子（life.PortableCore）。
//
// 设计要点（证明 World 独立性）：
//   - village 的 Rel 以 int64 为 key；导出时翻译成稳定的 Name，入境时再按本世界 idMap 翻回；
//     目标不在本世界则跳过（不强行织入）——这正说明“关系是否跨世界有意义”由世界自己决定。
//   - village.Agent 暂无 LifeState 字段，由 BaseAdapter 用 state 映射代管，不污染 village 结构体。
type VillageAdapter struct {
	*life.BaseAdapter
	w *World

	seq int64 // 旅行者本地 ID 区间起点（storeAllocLocal 自增）
}

// NewVillageAdapter 构造适配器，并快照当前所有 Agent 的 名字<->本地ID 映射。
func NewVillageAdapter(w *World) *VillageAdapter {
	a := &VillageAdapter{w: w, seq: 100000} // 旅行者本地 ID 区间，避开种子 1..N
	a.BaseAdapter = &life.BaseAdapter{}
	a.BaseAdapter.Init(a)
	return a
}

func (a *VillageAdapter) WorldKey() string { return "village" }

func (a *VillageAdapter) CanAccept(p life.AgentPortable) bool { return p.Identity.Name != "" }

// ---- life.PortableCore 钩子 ----

func (a *VillageAdapter) SeedNames() map[string]int64 {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	m := make(map[string]int64, len(a.w.agents))
	for id, ag := range a.w.agents {
		m[ag.Name] = id
	}
	return m
}

func (a *VillageAdapter) StoreGet(local int64) (any, bool) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	ag, ok := a.w.agents[local]
	return ag, ok
}

func (a *VillageAdapter) StorePut(local int64, ag any) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	a.w.agents[local] = ag.(*Agent)
	found := false
	for _, x := range a.w.agentOrd {
		if x == local {
			found = true
			break
		}
	}
	if !found {
		a.w.agentOrd = append(a.w.agentOrd, local)
	}
}

func (a *VillageAdapter) StoreRemove(local int64) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	delete(a.w.agents, local)
	for i, x := range a.w.agentOrd {
		if x == local {
			a.w.agentOrd = append(a.w.agentOrd[:i], a.w.agentOrd[i+1:]...)
			break
		}
	}
}

// 旅行者本地 ID 区间从 100000 起，避开种子 1..N。
func (a *VillageAdapter) StoreAllocLocal() int64 {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	a.seq++
	return a.seq
}

func (a *VillageAdapter) ExportLocked(local int64, ag any) (life.AgentPortable, error) {
	return a.exportAgent(local, ag.(*Agent)), nil
}

func (a *VillageAdapter) ImportLocked(local int64, p life.AgentPortable) (any, error) {
	return a.importAgent(local, p), nil
}

// ---- 世界特定的纯翻译 ----

// exportAgent: village.Agent -> AgentPortable（调用时已持 b.mu）。
func (a *VillageAdapter) exportAgent(local int64, ag *Agent) life.AgentPortable {
	st := ag.Life
	if st == "" {
		st = life.LifeAlive // 默认在世
	}
	pid := a.BaseAdapter.StableIDOf(local) // 携带不变的全局稳定身份（非按当前世界 local 重算）
	p := life.AgentPortable{
		AgentID: pid,
		Identity: life.PortableIdentity{
			ID:         pid,
			Name:       ag.Name,
			Occupation: ag.Occupation,
			Emoji:      ag.Emoji,
		},
		Personality: life.PortablePersonality{Traits: ag.Personality, Goal: ag.Goal},
		Life:        life.PortableLife{State: st, Energy: ag.Energy, Mood: ag.Mood},
	}
	if p.Life.State == "" {
		p.Life.State = life.LifeAlive
	}
	for k, v := range ag.Skills {
		p.Skills = append(p.Skills, life.PortableSkill{Name: k, Level: v})
	}
	for tid, rel := range ag.Rel {
		p.Relationships = append(p.Relationships, life.PortableRelationship{
			TargetID: life.StableID(a.WorldKey(), tid), Like: rel.Like, Trust: rel.Trust,
		})
	}
	for _, m := range ag.Mem {
		p.Memories = append(p.Memories, life.PortableMemory{
			Day: m.Day, Minute: m.Minute, Text: m.Text, Imp: m.Imp,
			Src: m.Src, Tag: m.Tag, EchoDay: m.EchoDay, Echoed: m.Echoed,
		})
	}
	if ag.Money != 0 {
		p.Assets = append(p.Assets, life.PortableAsset{Kind: "gold", Name: "gold", Qty: ag.Money})
	}
	return p
}

// importAgent: AgentPortable -> village.Agent（冻结过的 Agent 复用原本地 ID，关系可正确还原）。
func (a *VillageAdapter) importAgent(local int64, p life.AgentPortable) *Agent {
	ag := &Agent{
		ID:          local,
		Name:        p.Identity.Name,
		Occupation:  p.Identity.Occupation,
		Emoji:       p.Identity.Emoji,
		Personality: p.Personality.Traits,
		Money:       0,
		Energy:      70,
		Mood:        20,
		Skills:      map[string]int{},
		Goal:        p.Personality.Goal,
		Workplace:   "residential",
		Home:        "residential",
		Place:       "residential",
		ActKind:     "idle",
		Action:      "Just returned from another world",
		Life:        life.LifeAlive, // 默认在世；下方若携带 LifeState 则覆盖
		Rel:         map[int64]*Relationship{},
		Owes:        map[int64]int64{},
		Mem:         []Memory{},
		LastChatAt:  map[int64]int64{},
	}
	if p.Life.State != "" {
		ag.Life = p.Life.State // 携带回来的生命周期（alive/sleeping/...）
	}
	if p.Life.Energy != 0 {
		ag.Energy = p.Life.Energy
	}
	if p.Life.Mood != 0 {
		ag.Mood = p.Life.Mood
	}
	for _, s := range p.Skills {
		ag.Skills[s.Name] = s.Level
	}
	for _, r := range p.Relationships {
		tid, ok := a.BaseAdapter.LocalIDUnlocked(r.TargetID) // 目标不在本世界则跳过（World 独立性）
		if !ok {
			continue
		}
		ag.Rel[tid] = &Relationship{Like: r.Like, Trust: r.Trust}
	}
	for _, m := range p.Memories {
		ag.Mem = append(ag.Mem, Memory{
			Day: m.Day, Minute: m.Minute, Text: m.Text, Imp: m.Imp,
			Src: m.Src, Tag: m.Tag, EchoDay: m.EchoDay, Echoed: m.Echoed,
		})
	}
	for _, asset := range p.Assets {
		if asset.Kind == "gold" {
			ag.Money += asset.Qty
		}
	}
	return ag
}

// LocalIDSafe 把稳定 ID 解析为本地 int64（供 importAgent 内部使用，已在 b.mu 下调用）。
func (a *VillageAdapter) LocalIDSafe(pid life.AgentID) (int64, bool) {
	return a.BaseAdapter.LocalIDUnlocked(pid)
}
