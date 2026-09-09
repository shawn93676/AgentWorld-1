// Command m9 是 M9 “Life Runtime” 的首个端到端证明：
//
//	让 Marcus 在 Village（社交物理）与 Economy（资源物理）两个规则不同的世界里连续生活，
//	并证明往返之后他“还是 Marcus，但带着新经历”。
//
// 验收标准（M9）：一个 Agent，两个世界，连续生活，身份连续。
//
// 说明：本 demo 用 life.Move 真实跑通 Village↔Economy 的进出事务；Economy 内的“工作/赚钱”
// 通过调用经济世界的真实 Transfer / UpgradeSkill 完成，跨世界经历由 adapter 的 carry 携带。
// 把“自动产生跨世界目标”接进调度器是下一步。
package main

import (
	"fmt"

	"agentworld/internal/life"
	"agentworld/internal/skill"
	"agentworld/worlds/economy/economy"
	"agentworld/worlds/goosegame/goose"
	"agentworld/worlds/village/village"
)

func main() {
	obs := goose.NewObservatory(goose.ObservOpts{})

	// ---- 1. Village：Marcus（铁匠 Lv5，2 段记忆，347 金） ----
	vw := village.NewWorld(obs, "")
	vw.Attach(1, village.Profile{
		Name:        "Marcus",
		Occupation:  "Blacksmith",
		Emoji:       "🔨",
		Personality: []string{"hardworking", "proud"},
		Money:       347,
		Skills:      map[string]int{"blacksmith": 5},
		Goal:        "Be the best smith in Willow Creek",
		SeedMem:     []string{"Forged Sarah's plough.", "Won the summer smithing contest."},
	})
	vw.SealRelations()
	vadapter := village.NewVillageAdapter(vw)

	// Marcus 在 Village 的本地 ID 是 1（见上方 Attach(1, ...)），其全局稳定身份为 "village:1"。
	// 跨世界移动一律用稳定 ID 寻址，避免两个世界都有 "Marcus" 时在本世界 Enter 互相覆盖。
	marcusStable := life.StableID("village", 1)

	before := mustPortable(vadapter, marcusStable)
	fmt.Println("=== 出发前（Village）===")
	printPortable(before)

	// ---- 2. Economy：先有两个本地 Agent，再让 Marcus 入境 ----
	sk := skill.NewRegistry()
	ew := economy.NewWorld([]int64{1, 2}, []string{"Alice", "Bob"},
		[]string{"稳健，喜欢稳定收益", "勤劳，埋头苦干"}, obs, sk)
	eadapter := economy.NewEconomyAdapter(ew)

	// 注册中心：按 WorldKey 自注册，Move 即可按名字寻址（无需手动传 adapter 实例）。
	reg := life.NewRegistry()
	reg.Register(vadapter)
	reg.Register(eadapter)

	// 自由往返校验：每个新世界接好后都能白送一次“导出→导入→再导出”的严格一致性检查。
	// 注意：village 与 economy 的便携契约不同（village 携带 Energy/Mood、economy 导出时
	// 硬编码 Energy=100/Mood=0），所以这里给每个 adapter 喂符合其自身契约的样本，
	// 而不是用同一个样本跑 SelfTestAll（那只适用于共享同一份 portable 契约的世界）。
	if err := vadapter.SelfTest(life.AgentPortable{
		AgentID:  life.StableID("village", 1),
		Identity: life.PortableIdentity{ID: life.StableID("village", 1), Name: "Marcus", Occupation: "Blacksmith"},
		Personality: life.PortablePersonality{Traits: []string{"hardworking"}},
		Skills:   []life.PortableSkill{{Name: "blacksmith", Level: 5}},
		Life:     life.PortableLife{State: life.LifeAlive, Energy: 70, Mood: 20},
	}); err != nil {
		panic(fmt.Sprintf("village SelfTest: %v", err))
	}
	if err := eadapter.SelfTest(life.AgentPortable{
		AgentID:  life.StableID("economy", 1),
		Identity: life.PortableIdentity{ID: life.StableID("economy", 1), Name: "Marcus", Occupation: "Blacksmith"},
		Personality: life.PortablePersonality{Traits: []string{"hardworking"}},
		Skills:   []life.PortableSkill{{Name: "blacksmith", Level: 5}},
		Life:     life.PortableLife{State: life.LifeAlive, Energy: 100, Mood: 0},
	}); err != nil {
		panic(fmt.Sprintf("economy SelfTest: %v", err))
	}
	fmt.Println("=== SelfTest: village & economy 往返校验通过 ===")

	// Marcus 自己产生目标：“去 Economy World 工作” → 跨 World 移动
	t1, err := reg.MoveByKey("village", "economy", marcusStable)
	if err != nil {
		panic(err)
	}
	fmt.Printf("\n=== Move Village→Economy: %s ===\n", t1.Status)

	// ---- 3. 在 Economy 工作 / 赚钱（真实调用经济世界接口） ----
	mid, _ := eadapter.LocalID(marcusStable)
	ea := ew.Agent(mid)
	ew.Transfer(0, ea.ID, 120, "job-reward", "Worked as a smith for a day")
	ea.UpgradeSkill("blacksmith") // Lv5 → Lv6：技能随工作演化
	fmt.Printf("Economy 内 Marcus: Balance=%d, Skills=%v\n", ea.Balance, ea.Skills)

	// 记录“在 Economy 工作”的跨世界经历（经济世界原生无记忆字段，由 adapter carry 携带）
	eadapter.NoteExperience(marcusStable, life.PortableMemory{
		Day: 1, Minute: 0, Text: "Worked in the Economy World and earned 120 gold as a smith.", Imp: 5,
	})

	// ---- 4. 带着“人生”回到 Village ----
	t2, err := reg.MoveByKey("economy", "village", marcusStable)
	if err != nil {
		panic(err)
	}
	fmt.Printf("=== Move Economy→Village: %s ===\n", t2.Status)

	// ---- 5. 证明“还是 Marcus，但带着新经历” ----
	after := mustPortable(vadapter, marcusStable)
	fmt.Println("\n=== 回家后（Village）===")
	printPortable(after)

	fmt.Println("\n=== 连续性断言 ===")
	ok := after.AgentID == before.AgentID &&
		after.Identity.Name == before.Identity.Name &&
		len(after.Memories) > len(before.Memories)
	fmt.Printf("身份连续(Marcus): %v\n", after.AgentID == before.AgentID)
	fmt.Printf("稳定身份(%s 不变): %v\n", marcusStable, after.AgentID == marcusStable)
	fmt.Printf("技能演化(铁匠 Lv5→Lv%d): %v\n", skillLevelOf(after), after.Skills[0].Level > before.Skills[0].Level)
	fmt.Printf("记忆增长(带新经历): %v (%d -> %d)\n",
		len(after.Memories) > len(before.Memories), len(before.Memories), len(after.Memories))
	if !ok {
		fmt.Println("M9 FAIL")
	} else {
		fmt.Println("M9 PASS — 他真的把自己的人生带过去了。")
	}
}

func mustPortable(a *village.VillageAdapter, id string) life.AgentPortable {
	v, err := a.Leave(id) // 仅导出快照；下方重新 Enter 还原
	if err != nil {
		panic(err)
	}
	if err := a.Enter(v); err != nil {
		panic(err)
	}
	return v
}

func skillLevelOf(p life.AgentPortable) int {
	if len(p.Skills) == 0 {
		return 0
	}
	return p.Skills[0].Level
}

func printPortable(p life.AgentPortable) {
	fmt.Printf("AgentID : %s\n", p.AgentID)
	fmt.Printf("Name    : %s (%s)\n", p.Identity.Name, p.Identity.Occupation)
	fmt.Printf("Skills  : ")
	for _, s := range p.Skills {
		fmt.Printf("%s Lv%d ", s.Name, s.Level)
	}
	fmt.Println()
	fmt.Printf("Memories: %d 条\n", len(p.Memories))
	for i, m := range p.Memories {
		if i >= 4 {
			fmt.Println("  …")
			break
		}
		fmt.Printf("  - %s\n", m.Text)
	}
	for _, asset := range p.Assets {
		if asset.Kind == "gold" {
			fmt.Printf("Gold    : %d\n", asset.Qty)
		}
	}
	fmt.Printf("Life    : %s\n", p.Life.State)
}
