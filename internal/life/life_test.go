package life_test

import (
	"testing"

	"agentworld/internal/life"
)

// 以下 mockCore 是“世界开发者接跨世界能力”的最小模板：
// 嵌入 *life.BaseAdapter 并实现 life.PortableCore 的钩子，即可免费获得
// Leave/Enter/Export/LocalID/SelfTest 与 Move 事务。复制本文件改为你自己的
// Agent 类型即可。

type mockAgent struct {
	name   string
	skills map[string]int
}

type mockCore struct {
	*life.BaseAdapter
	store map[int64]mockAgent
	seq   int64
}

func newMock() *mockCore {
	m := &mockCore{store: map[int64]mockAgent{}, seq: 1}
	m.BaseAdapter = &life.BaseAdapter{}
	m.BaseAdapter.Init(m)
	return m
}

func (m *mockCore) WorldKey() string { return "mock" }
func (m *mockCore) CanAccept(p life.AgentPortable) bool {
	return p.Identity.Name != ""
}

func (m *mockCore) SeedNames() map[string]int64 {
	mm := map[string]int64{}
	for id, a := range m.store {
		mm[a.name] = id
	}
	return mm
}
func (m *mockCore) StoreGet(local int64) (any, bool) {
	a, ok := m.store[local]
	return a, ok
}
func (m *mockCore) StorePut(local int64, ag any) { m.store[local] = ag.(mockAgent) }
func (m *mockCore) StoreRemove(local int64)      { delete(m.store, local) }
func (m *mockCore) StoreAllocLocal() int64 {
	m.seq++
	return m.seq
}
func (m *mockCore) ExportLocked(local int64, ag any) (life.AgentPortable, error) {
	a := ag.(mockAgent)
	p := life.AgentPortable{
		AgentID:  a.name,
		Identity: life.PortableIdentity{ID: a.name, Name: a.name},
		Life:     life.PortableLife{State: m.BaseAdapter.StateOf(local)},
	}
	for k, v := range a.skills {
		p.Skills = append(p.Skills, life.PortableSkill{Name: k, Level: v})
	}
	return p, nil
}
func (m *mockCore) ImportLocked(local int64, p life.AgentPortable) (any, error) {
	a := mockAgent{name: p.Identity.Name, skills: map[string]int{}}
	for _, s := range p.Skills {
		a.skills[s.Name] = s.Level
	}
	return a, nil
}

func TestSelfTestRoundTrip(t *testing.T) {
	m := newMock()
	sample := life.AgentPortable{
		AgentID:  "Marcus",
		Identity: life.PortableIdentity{ID: "Marcus", Name: "Marcus"},
		Skills:   []life.PortableSkill{{Name: "blacksmith", Level: 5}},
		Life:     life.PortableLife{State: life.LifeAlive},
	}
	if err := m.SelfTest(sample); err != nil {
		t.Fatalf("SelfTest failed: %v", err)
	}
}

func TestMoveRoundTrip(t *testing.T) {
	a := newMock()
	b := newMock()
	sample := life.AgentPortable{
		AgentID:  "Marcus",
		Identity: life.PortableIdentity{ID: "Marcus", Name: "Marcus"},
		Skills:   []life.PortableSkill{{Name: "blacksmith", Level: 5}},
		Life:     life.PortableLife{State: life.LifeAlive},
	}
	// Marcus 先进入世界 A
	if err := a.Enter(sample); err != nil {
		t.Fatalf("enter A: %v", err)
	}
	la, ok := a.LocalID("Marcus")
	if !ok {
		t.Fatal("Marcus not in A")
	}
	// A → B
	t1, err := life.Move(a, b, "Marcus")
	if err != nil || t1.Status != life.TransitionCompleted {
		t.Fatalf("move A→B failed: %v %s", err, t1.Status)
	}
	if _, ok := a.StoreGet(la); ok {
		t.Fatal("Marcus should have left A's store (id 映射保留以便回村复用本地槽位)")
	}
	if _, ok := b.LocalID("Marcus"); !ok {
		t.Fatal("Marcus should be in B")
	}
	// B → A：Marcus 回村，复用原本地槽位 la
	t2, err := life.Move(b, a, "Marcus")
	if err != nil || t2.Status != life.TransitionCompleted {
		t.Fatalf("move B→A failed: %v %s", err, t2.Status)
	}
	if _, ok := a.StoreGet(la); !ok {
		t.Fatal("Marcus should be back in A's store")
	}
}
