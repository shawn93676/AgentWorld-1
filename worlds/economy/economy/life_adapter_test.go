package economy

import (
	"testing"

	"agentworld/internal/life"
)

// newBridgeTestAdapter 构造一个最小 EconomyAdapter（仅用于技能桥单测，不依赖完整 World）。
func newBridgeTestAdapter() *EconomyAdapter {
	w := &World{Agents: map[int64]*Agent{}}
	return NewEconomyAdapter(w)
}

// blacksmithPortable 模拟从 Village 来的铁匠 Marcus：村庄尺度 Blacksmithing Lv72，金币 347。
func blacksmithPortable() life.AgentPortable {
	id := life.StableID("village", 1)
	return life.AgentPortable{
		AgentID: id,
		Identity: life.PortableIdentity{
			ID:         id,
			Name:       "Marcus",
			Occupation: "Blacksmith",
		},
		Personality: life.PortablePersonality{Traits: []string{"勤劳"}, Goal: "改善经济状况"},
		Life:        life.PortableLife{State: life.LifeAlive},
		Skills:      []life.PortableSkill{{Name: "Blacksmithing", Level: 72}},
		Assets:      []life.PortableAsset{{Kind: "gold", Name: "gold", Qty: 347}},
	}
}

func assetGold(as []life.PortableAsset) int64 {
	var g int64
	for _, a := range as {
		if a.Kind == "gold" {
			g += a.Qty
		}
	}
	return g
}

// TestSkillBridge_ImportExportRoundTrip
// Blacksmithing Lv72 → Economy import → engineer Lv5 → export → Blacksmithing Lv68（原样往返，规范技能名不变）
func TestSkillBridge_ImportExportRoundTrip(t *testing.T) {
	a := newBridgeTestAdapter()
	p := blacksmithPortable()

	if err := a.Enter(p); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	local, ok := a.LocalID(p.AgentID)
	if !ok {
		t.Fatal("agent not registered after Enter")
	}
	raw, _ := a.StoreGet(local)
	ag := raw.(*Agent)

	// 经济世界内部只认 engineer，绝不认 blacksmith
	if !ag.HasSkill("engineer") {
		t.Fatalf("economy agent should have engineer skill, got %+v", ag.Skills)
	}
	if ag.SkillLevel("engineer") != villageToEconLevel(72) {
		t.Fatalf("engineer level should be %d, got %d", villageToEconLevel(72), ag.SkillLevel("engineer"))
	}
	if ag.HasSkill("Blacksmithing") || ag.HasSkill("blacksmith") {
		t.Fatalf("economy must NOT know blacksmith, got %+v", ag.Skills)
	}

	out, err := a.Leave(p.AgentID)
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}
	// 规范技能（Blacksmithing）与职业原样往返；等级按尺度桥折算回村庄尺度。
	if len(out.Skills) != 1 || out.Skills[0].Name != "Blacksmithing" {
		t.Fatalf("export should restore Blacksmithing, got %+v", out.Skills)
	}
	if out.Skills[0].Level != econToVillageLevel(villageToEconLevel(72)) {
		t.Fatalf("export level should be %d, got %d", econToVillageLevel(villageToEconLevel(72)), out.Skills[0].Level)
	}
	if out.Identity.Occupation != "Blacksmith" {
		t.Fatalf("occupation should stay Blacksmith, got %q", out.Identity.Occupation)
	}
	if g := assetGold(out.Assets); g != 347 {
		t.Fatalf("gold should round-trip 347, got %d", g)
	}
}

// TestSkillBridge_WorkUpgradesLevel
// Blacksmithing Lv72 → Economy import → engineer Lv5 → work(UpgradeSkill) → engineer Lv6 → export → Blacksmithing Lv83
func TestSkillBridge_WorkUpgradesLevel(t *testing.T) {
	a := newBridgeTestAdapter()
	p := blacksmithPortable()
	if err := a.Enter(p); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	local, _ := a.LocalID(p.AgentID)
	raw, _ := a.StoreGet(local)
	ag := raw.(*Agent)

	// 模拟 Economy 工作：DoJob 内部会调用 UpgradeSkill(engineer)，等级随之提升
	ag.UpgradeSkill("engineer")
	if ag.SkillLevel("engineer") != villageToEconLevel(72)+1 {
		t.Fatalf("after work engineer should be Lv%d, got %d", villageToEconLevel(72)+1, ag.SkillLevel("engineer"))
	}

	out, err := a.Leave(p.AgentID)
	if err != nil {
		t.Fatalf("Leave: %v", err)
	}
	// 升级后的等级必须正确带回到村庄规范技能（且比出发时更高）
	want := econToVillageLevel(villageToEconLevel(72) + 1)
	if len(out.Skills) != 1 || out.Skills[0].Name != "Blacksmithing" || out.Skills[0].Level != want {
		t.Fatalf("export should restore Blacksmithing Lv%d, got %+v", want, out.Skills)
	}
	if out.Skills[0].Level <= 72 {
		t.Fatalf("returned village level should be higher than 72, got %d", out.Skills[0].Level)
	}
}
