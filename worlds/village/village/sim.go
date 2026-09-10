package village

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// sim.go —— 世界推进（Tick）、规则决策（Level 0）、相遇互动、玩家干预。
//
// 成本模型（g1.txt §7）：日常行为全部走纯规则（Level 0），只有玩家对话
// 在配置了 LLM 时走高级模型（Level 2），Influence 接受率本地计算。

// activity 一次决策出的活动。
type Activity struct {
	Kind   string // work / sleep / eat / social / rest / shop / idle / repair
	Place  string
	At     int64  // 绝对分钟（开始）
	Until  int64  // 绝对分钟（结束）
	Text   string // 给人看的动作
	Spend  int64  // 立即支出
	Reason []string
	TalkTo string // social 特殊目标（Influence 计划用）
	Cross  bool   // 跨世界出行（目标非本村地点，需经 Mover 真正移动）
}

// ---- 感知（模块层用） ----

// Perception Agent 一轮的感知视图（G0 §7）。
type Perception struct {
	Now     int64      `json:"now"`
	Day     int        `json:"day"`
	Clock   string     `json:"clock"`
	Weather string     `json:"weather"`
	Me      AgentBrief `json:"me"`
	Nearby  []string   `json:"nearby"`
	Plan    *Plan      `json:"plan,omitempty"`
	Repair  bool       `json:"repair_needed"`
}

// AgentBrief 前端/感知共用的轻量视图。
type AgentBrief struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Occupation string `json:"occupation"`
	Emoji      string `json:"emoji"`
	Money      int64  `json:"money"`
	Energy     int    `json:"energy"`
	Mood       int    `json:"mood"`
	MoodEmoji  string `json:"mood_emoji"`
	Place      string `json:"place"`
	Action     string `json:"action"`
	ActKind    string `json:"act_kind"`
	Traveling  bool   `json:"traveling"`
	TravelTo   string `json:"travel_to,omitempty"` // 旅行目的地（前端画移动中的村民）
	Goal       string `json:"goal"`
	GoalTarget int64  `json:"goal_target"`
	GoalDone   bool   `json:"goal_done"`
	Gone       bool   `json:"gone"` // 已离开村庄（不可逆）
}

func (w *World) brief(a *Agent) AgentBrief {
	return AgentBrief{
		ID: a.ID, Name: a.Name, Occupation: a.Occupation, Emoji: a.Emoji,
		Money: a.Money, Energy: a.Energy, Mood: a.Mood, MoodEmoji: MoodEmoji(a.Mood),
		Place: a.Place, Action: a.Action, ActKind: a.ActKind,
		Traveling: a.ActKind == "travel", TravelTo: a.TravelTo,
		Goal: a.Goal, GoalTarget: a.GoalTarget, GoalDone: a.GoalDone, Gone: a.Gone,
	}
}

// Perceive 构建感知。
func (w *World) Perceive(id int64) *Perception {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.agents[id]
	if a == nil {
		return nil
	}
	p := &Perception{Now: w.now, Day: w.Day(), Clock: w.Clock(), Weather: w.weather,
		Me: w.brief(a), Plan: a.Plan, Repair: w.repair}
	for _, oid := range w.agentOrd {
		o := w.agents[oid]
		if o.ID != a.ID && !o.Gone && o.Place == a.Place && o.ActKind != "sleep" {
			p.Nearby = append(p.Nearby, o.Name)
		}
	}
	return p
}

// ---- 世界推进 ----

// Tick 推进一个世界节拍（由 main 的 ticker 驱动；整个方法持锁 = Village Actor 串行语义）。
func (w *World) Tick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tickN++
	prevDay := w.Day()
	w.now += int64(w.speed)

	if day := w.Day(); day != prevDay {
		w.weather = w.pickWeather()
		w.addEvent(Event{Type: "day", Icon: "🌅",
			Text: fmt.Sprintf("Day %d dawns over Willow Creek. The sky is %s.", day, w.weather),
			Why:  []string{"A new day", fmt.Sprintf("Weather roll: %s", w.weather)}})
		// 每日房租（G0 §13 支出项）
		for _, id := range w.agentOrd {
			a := w.agents[id]
			if a.Gone || (a.Life != "" && !a.Life.IsActive()) {
				continue // 已离开 / 非在世（死亡·归档）者不缴租
			}
			a.Money -= 5
			a.rentPaidDay = day
			if a.Money < 0 {
				a.Mood -= 8
				a.RentMisses++
				w.remember(a, "I couldn't pay rent yesterday. The pouch is empty.", 3)
			}
		}
		w.addEvent(Event{Type: "economy", Icon: "🏠",
			Text: "The village collected its daily rent (5 gold each).",
			Why:  []string{"Daily expense", "Keeps the economy moving"}})
		w.runEchoesLocked()  // 长程因果：玩家过去的干预在若干天后回响
		w.checkFatesLocked() // 稀缺一侧：债务与绝望会把人逼走（不可逆）
		w.checkGoalsLocked() // 目标一侧：绝望或长期停滞会让村民放弃目标（可逆）
	}

	// 能量/心情漂移 + 活动完成结算
	for _, id := range w.agentOrd {
		a := w.agents[id]
		if a.Gone || (a.Life != "" && !a.Life.IsActive()) {
			continue // 已离开村庄 / 非在世者不再参与世界推进
		}
		switch a.ActKind {
		case "sleep":
			a.Energy += 8
			a.Mood++
		case "work":
			a.Energy -= 3
			if a.Energy < 20 {
				a.Mood -= 2
			}
		case "repair":
			a.Energy -= 3
		case "travel":
			a.Energy -= 2
		case "social":
			a.Mood += 2
			a.Energy--
		case "eat", "shop":
			a.Energy += 3
			a.Mood += 2
		case "rest":
			a.Energy += 5
		}
		a.Energy = clamp(a.Energy, 0, 100)
		a.Mood = clamp(a.Mood, -100, 100)

		switch {
		case a.ActKind == "travel" && w.now >= a.ActionUntil:
			// 跨世界出行：到达的是另一个 World，由注入的 Mover 真正发起 MoveRemote，
			// 并更新 LifeState（离开本村）。成功后本 Agent 不再参与本世界推进。
			if a.Crossing {
				a.Crossing = false
				dest := a.TravelTo
				a.TravelTo = ""
				if w.mover != nil {
					// 不能在 Tick 持锁期间直接发起跨世界 MoveRemote（adapter.Leave 会回锁
					// World，造成死锁）。改为脱离锁的 goroutine 执行，并标记 ActKind 防止 Tick
					// 在本 tick 内重复处理该 Agent。
					a.ActKind = "departing"
					name, localID, toWorld := a.Name, a.ID, dest
					go func() {
						if err := w.mover(localID, toWorld); err == nil {
							w.EmitEvent("departure", "🚀", name,
								"Blacksmith "+name+" leaves the village for the Economy world",
								"decided to seek work and skill abroad")
						} else {
							w.EmitEvent("departure", "⚠️", name,
								name+" wanted to leave but the road was closed",
								err.Error())
							w.mu.Lock()
							a.ActKind = "idle"
							a.Due = true
							w.mu.Unlock()
						}
					}()
				} else {
					// 未注入跨世界 Mover：本次不跨世界，回到可决策状态稍后重试。
					w.EmitEvent("departure", "⚠️", a.Name, a.Name+" wanted to leave but cross-world is not configured")
					a.ActKind = "idle"
					a.Due = true
				}
				break
			}
			// 本地到达
			a.Place = a.TravelTo
			a.TravelTo = ""
			dest := w.placeName(a.Place)
			w.addEvent(Event{Type: "move", Icon: "📍", Actor: a.Name, Place: a.Place,
				Text: fmt.Sprintf("%s arrived at the %s.", a.Name, dest)})
			if a.NextAct != nil {
				act := a.NextAct
				a.NextAct = nil
				w.applyAct(a, act, true)
			} else {
				a.Due = true
			}
		case w.now >= a.ActionUntil:
			if a.ActKind == "work" {
				w.settleWork(a)
			}
			if a.ActKind == "repair" {
				w.settleRepair(a)
			}
			a.Due = true
		}
	}

	w.encounters()
	w.flavor()
	w.turnThreadsLocked() // 悬念线程：结算到期的 + 补充未决的

	if w.tickN%36 == 0 { // ~每 3 分钟自动存档
		_ = w.saveLocked()
	}
	if w.obs != nil {
		w.obs.Publish("village.state", w.stateLocked())
	}
}

// settleWork 完成一段工作的结算：发工资 + 目标进度。
func (w *World) settleWork(a *Agent) {
	wage := w.wageFor(a)
	a.Money += wage
	why := []string{
		fmt.Sprintf("Occupation: %s", a.Occupation),
		fmt.Sprintf("Worked a full stint at the %s", w.placeName(a.Workplace)),
	}
	if a.GoalTarget > 0 && !a.GoalDone && !a.GoalAbandoned {
		why = append(why, fmt.Sprintf("Saving for \"%s\" (%d/%d gold)", a.Goal, a.Money, a.GoalTarget))
	}
	w.addEvent(Event{Type: "economy", Icon: "💰", Actor: a.Name, Place: a.Place,
		Text: fmt.Sprintf("%s finished a stint of work and earned %d gold.", a.Name, wage),
		Why:  why})
	if a.GoalTarget > 0 && !a.GoalDone && !a.GoalAbandoned && a.Money >= a.GoalTarget {
		a.GoalDone = true
		tr := &WhyTrace{
			Rule:     "goal.completed",
			Cause:    []string{fmt.Sprintf("money=%d (target %d)", a.Money, a.GoalTarget), "savings reached threshold"},
			Recent:   []string{fmt.Sprintf("Saving for \"%s\" (%d/%d gold)", a.Goal, a.Money, a.GoalTarget)},
			Decision: fmt.Sprintf("goal completed: %s", a.Goal),
		}
		w.addEvent(Event{Type: "milestone", Icon: "🎉", Actor: a.Name, Place: a.Place,
			Text: fmt.Sprintf("%s has finally saved enough to %s!", a.Name, lowerFirst(a.Goal)),
			Why: []string{fmt.Sprintf("Goal reached: %d gold", a.GoalTarget),
				"Months of honest work"},
			Trace: tr})
		w.remember(a, fmt.Sprintf("I finally saved %d gold. My dream is within reach!", a.GoalTarget), 5)
		w.logWhy(a.Name, "goal.completed", tr, a.Goal)
	}
}

// checkGoalsLocked 每日检查：村民是否该放弃目标（goal.abandoned）。
// 与离村不同，放弃是可逆的——一笔横财或心情回暖、重新攒钱，都能重燃希望。
func (w *World) checkGoalsLocked() {
	if w.Day() <= fateGraceDays {
		return
	}
	for _, id := range w.agentOrd {
		a := w.agents[id]
		if a == nil || a.Gone || a.GoalTarget <= 0 || a.GoalDone {
			continue
		}
		// 已放弃者：检查是否因时来运转而重燃希望。
		if a.GoalAbandoned {
			if a.Money >= a.GoalTarget {
				a.GoalAbandoned, a.goalBest, a.goalStallDays = false, a.Money, 0
			} else if a.Mood > giveUpMood && a.Money > a.goalBest {
				a.GoalAbandoned, a.goalBest, a.goalStallDays = false, a.Money, 0
			}
			continue
		}
		if a.Money >= a.GoalTarget {
			continue // 已完成由 settleWork 处理
		}
		// 进度判定：刷新历史最高积蓄则算有进展，否则停滞天数 +1。
		if a.Money > a.goalBest {
			a.goalBest, a.goalStallDays = a.Money, 0
		} else {
			a.goalStallDays++
		}
		gap := a.GoalTarget - a.Money
		// 判定触发线：绝望 or 停滞。
		trigger := ""
		switch {
		case a.Mood <= giveUpMood:
			trigger = "hopelessness"
		case a.goalStallDays >= giveUpStall:
			trigger = "stagnation"
		}
		if trigger == "" {
			continue
		}
		w.abandonGoalLocked(a, trigger, gap)
	}
}

// abandonGoalLocked 村民放弃目标（goal.abandoned）。产出结构化因果链，便于事后追溯。
func (w *World) abandonGoalLocked(a *Agent, trigger string, gap int64) {
	a.GoalAbandoned = true
	tr := &WhyTrace{
		Rule: "goal.abandoned",
		Cause: []string{
			fmt.Sprintf("money=%d (target %d, gap %d)", a.Money, a.GoalTarget, gap),
			fmt.Sprintf("mood=%d (give-up line %d)", a.Mood, giveUpMood),
			fmt.Sprintf("no_progress_days=%d (give-up line %d)", a.goalStallDays, giveUpStall),
			"triggered_by=" + trigger,
		},
		Recent:   recentCauses(a),
		Decision: fmt.Sprintf("goal abandoned: %s — no longer pursued (reversible by a change in fortune)", a.Goal),
	}
	w.addEvent(Event{Type: "despair", Icon: "💔", Actor: a.Name, Place: a.Place,
		Text: fmt.Sprintf("%s has given up on %s. The dream is set aside, perhaps forever.", a.Name, lowerFirst(a.Goal)),
		Why: []string{"Lost hope of reaching the goal",
			fmt.Sprintf("Saved %d of %d gold · Mood %s", a.Money, a.GoalTarget, MoodEmoji(a.Mood))},
		Trace: tr})
	w.remember(a, fmt.Sprintf("I've stopped chasing %s. It was never going to happen.", lowerFirst(a.Goal)), 4)
	w.logWhy(a.Name, "goal.abandoned", tr, a.Goal)
}

// settleRepair 修好 Market。
func (w *World) settleRepair(a *Agent) {
	if !w.repair {
		return
	}
	w.repair = false
	reward := int64(12)
	if b := w.agentByName("Bob"); b != nil && b.Money >= reward {
		b.Money -= reward
		a.Money += reward
	} else {
		reward = 0
	}
	w.addEvent(Event{Type: "milestone", Icon: "🔨", Actor: a.Name, Place: a.Place,
		Text: fmt.Sprintf("%s repaired the fire-damaged Market stalls.%s", a.Name,
			ternary(reward > 0, fmt.Sprintf(" Bob paid %d gold for the work.", reward), "")),
		Why: []string{"Blacksmithing skill 72", "Protect local trade",
			"The Market burned down yesterday"}})
	for _, id := range w.agentOrd {
		o := w.agents[id]
		if o.Name == a.Name {
			continue
		}
		if o.Place == "market" || o.Name == "Bob" || o.Name == "Emma" {
			w.adjustRel(o, a, 6, 5)
			w.remember(o, fmt.Sprintf("%s repaired the Market after the fire. Good man.", a.Name), 4)
		}
	}
}

// encounters 相遇互动（同一地点、非旅行、清醒的两个村民）。
func (w *World) encounters() {
	type pairKey struct{ a, b int64 }
	done := map[pairKey]bool{}
	n := 0
	for _, pid := range w.placeOrd {
		if n >= 3 {
			break
		}
		var here []*Agent
		for _, id := range w.agentOrd {
			a := w.agents[id]
			if a.Place == pid && a.ActKind != "travel" && a.ActKind != "sleep" && !a.Gone {
				here = append(here, a)
			}
		}
		if len(here) < 2 {
			continue
		}
		for i := 0; i < len(here) && n < 3; i++ {
			for j := i + 1; j < len(here) && n < 3; j++ {
				a, b := here[i], here[j]
				if a.ID > b.ID {
					a, b = b, a
				}
				k := pairKey{a.ID, b.ID}
				if done[k] {
					continue
				}
				done[k] = true
				if w.now-a.LastChatAt[b.ID] < 240 {
					continue
				}
				p := 0.20 * (a.Social + b.Social)
				if w.rng.Float64() >= p {
					continue
				}
				if w.interact(a, b) {
					n++
				}
			}
		}
	}
}

// interact 两个村民的互动；返回是否产生了事件。
func (w *World) interact(a, b *Agent) bool {
	a.LastChatAt[b.ID] = w.now
	b.LastChatAt[a.ID] = w.now
	place := w.placeName(a.Place)
	hour := w.Minute() / 60

	// 债务：先处理钱的事（G0 的核心冲突素材）
	if creditor, debtor, amt := w.debtBetween(a, b); amt > 0 {
		if debtor.Money >= amt+20 && w.rng.Float64() < 0.6 {
			debtor.Money -= amt
			creditor.Money += amt
			delete(debtor.Owes, creditor.ID)
			w.adjustRel(creditor, debtor, 12, 15)
			w.adjustRel(debtor, creditor, 6, 10)
			w.remember(creditor, fmt.Sprintf("%s finally repaid the %d gold I was owed.", debtor.Name, amt), 4)
			w.remember(debtor, fmt.Sprintf("I repaid %s the %d gold. A weight off my shoulders.", creditor.Name, amt), 4)
			w.addEvent(Event{Type: "social", Icon: "🤝", Actor: debtor.Name, Target: creditor.Name, Place: a.Place,
				Text: fmt.Sprintf("%s repaid %s the %d gold he owed. Their friendship mended a little.",
					debtor.Name, creditor.Name, amt),
				Why: []string{fmt.Sprintf("Debt of %d gold cleared", amt), "Debtor finally had the money"}})
			return true
		}
		if w.rng.Float64() < 0.5 {
			w.addEvent(Event{Type: "conflict", Icon: "⚡", Actor: creditor.Name, Target: debtor.Name, Place: a.Place,
				Text: fmt.Sprintf("%s confronted %s about the %d gold still owed.", creditor.Name, debtor.Name, amt),
				Why: []string{fmt.Sprintf("Unpaid debt: %d gold", amt),
					fmt.Sprintf("%s is %s", creditor.Name, lowerFirst(strings.Join(creditor.Personality, " and ")))}})
			w.adjustRel(creditor, debtor, -3, -2)
			w.adjustRel(debtor, creditor, -2, -3)
			creditor.Mood -= 5
			debtor.Mood -= 10
			w.remember(debtor, fmt.Sprintf("%s is getting angry about the %d gold I owe.", creditor.Name, amt), 3)
			return true
		}
	}

	// 酒馆午餐
	if a.Place == "tavern" && hour >= 11 && hour <= 14 && a.Money >= 6 && b.Money >= 6 {
		a.Money -= 6
		b.Money -= 6
		a.Mood += 8
		b.Mood += 8
		w.adjustRel(a, b, 4, 2)
		w.adjustRel(b, a, 4, 2)
		w.remember(a, fmt.Sprintf("Had lunch with %s at the Tavern.", b.Name), 2)
		w.remember(b, fmt.Sprintf("Had lunch with %s at the Tavern.", a.Name), 2)
		w.addEvent(Event{Type: "social", Icon: "🍽️", Actor: a.Name, Target: b.Name, Place: a.Place,
			Text: fmt.Sprintf("%s and %s shared a warm lunch at the Tavern.", a.Name, b.Name),
			Why:  []string{"Lunchtime at the Tavern", "Both had coin for a meal"}})
		return true
	}

	// Mary 关心 John 的店铺梦（G0 §5 的素材）
	if (a.Name == "Mary" && b.Name == "John") || (a.Name == "John" && b.Name == "Mary") {
		if a.Place == "tavern" && w.rng.Float64() < 0.35 {
			john := a
			if john.Name != "John" {
				john = b
			}
			john.Mood += 3
			w.remember(john, "Mary asked how my shop fund is coming along. She believes in me.", 3)
			w.addEvent(Event{Type: "social", Icon: "💭", Actor: "Mary", Target: "John", Place: a.Place,
				Text: "Mary asked John how his shop fund is coming along.",
				Why:  []string{"Mary and John are close friends", "She knows his dream"}})
			return true
		}
	}

	// Bob 在市场做买卖
	if place == "Market" && (a.Name == "Bob" || b.Name == "Bob") && w.rng.Float64() < 0.5 {
		profit := int64(5 + w.rng.Intn(10))
		bob := a
		if bob.Name != "Bob" {
			bob = b
		}
		bob.Money += profit
		w.addEvent(Event{Type: "economy", Icon: "💰", Actor: "Bob", Place: a.Place,
			Text: fmt.Sprintf("Bob haggled a fine deal at the Market and made %d gold.", profit),
			Why:  []string{"Haggling skill 62", "Caravan goods sold at margin"}})
		return true
	}

	// 普通闲聊
	d := 1 + w.rng.Intn(3)
	w.adjustRel(a, b, d, 1)
	w.adjustRel(b, a, d, 1)
	w.remember(a, fmt.Sprintf("Chatted with %s at the %s.", b.Name, place), 1)
	w.remember(b, fmt.Sprintf("Chatted with %s at the %s.", a.Name, place), 1)
	w.addEvent(Event{Type: "social", Icon: "💬", Actor: a.Name, Target: b.Name, Place: a.Place,
		Text: fmt.Sprintf("%s and %s chatted at the %s.", a.Name, b.Name, place),
		Why:  []string{"Both were at the " + place, "Friendly villagers"}})
	return true
}

// flavor 随机风味事件（职业彩蛋 + 罕见大火）。
func (w *World) flavor() {
	if w.rng.Float64() < 0.03 {
		var cands []*Agent
		for _, id := range w.agentOrd {
			a := w.agents[id]
			if !a.Gone && (a.ActKind == "work" || a.ActKind == "social" || a.ActKind == "idle") {
				cands = append(cands, a)
			}
		}
		if len(cands) > 0 {
			a := cands[w.rng.Intn(len(cands))]
			w.occupationFlavor(a)
		}
	}
	// 罕见大事件：Market 失火（G0 §10 大事件）
	if !w.repair && w.Minute()/60 >= 10 && w.Minute()/60 <= 20 && w.rng.Float64() < 0.002 {
		w.repair = true
		text := "A cooking fire broke out at the Market! Smoke rises over the stalls."
		if bob := w.agentByName("Bob"); bob != nil {
			loss := min64(bob.Money, 20)
			bob.Money -= loss
			bob.Mood -= 25
			w.remember(bob, fmt.Sprintf("The fire destroyed %d gold of my goods.", loss), 5)
			text = fmt.Sprintf("A cooking fire broke out at the Market! Bob lost %d gold of goods.", loss)
		}
		for _, id := range w.agentOrd {
			o := w.agents[id]
			if o.Place == "market" && o.Name != "Bob" {
				o.Mood -= 10
				w.remember(o, "The Market caught fire today. Terrifying.", 3)
			}
		}
		w.addEvent(Event{Type: "disaster", Icon: "🔥", Place: "market",
			Text: text,
			Why:  []string{"A knocked-over cooking brazier", "Wooden stalls burn fast"}})
	}
}

// occupationFlavor 职业风味事件。
func (w *World) occupationFlavor(a *Agent) {
	type flav struct {
		icon, text string
		mood       int
		money      int64
	}
	var f flav
	switch a.Occupation {
	case "Blacksmith":
		f = flav{"⚒️", fmt.Sprintf("%s forged a batch of fine nails at the forge.", a.Name), 3, 0}
	case "Innkeeper":
		f = flav{"🍺", "A traveling bard checked into the Tavern and started playing.", 4, 0}
	case "Merchant":
		f = flav{"📦", "A caravan arrived at the Market with new goods.", 2, 0}
	case "Farmer":
		f = flav{"🌾", fmt.Sprintf("%s's wheat swayed in the breeze — a good harvest is coming.", a.Name), 3, 0}
	case "Healer":
		f = flav{"🌿", fmt.Sprintf("%s found a patch of moon-herb near the Church.", a.Name), 4, 0}
	case "Baker":
		f = flav{"🥖", fmt.Sprintf("%s pulled honey bread from the oven; the smell drifted across the Market.", a.Name), 3, 0}
	case "Miner":
		if w.rng.Float64() < 0.4 {
			f = flav{"⛏️", fmt.Sprintf("%s struck a rich vein of iron in the deep tunnel.", a.Name), 8, 10}
		} else {
			f = flav{"⛏️", fmt.Sprintf("%s hauled another cart of ore up from the Mine.", a.Name), 0, 0}
		}
	case "Priest":
		f = flav{"🔔", fmt.Sprintf("%s rang the church bell for evening prayer.", a.Name), 2, 0}
	case "Guard":
		f = flav{"🛡️", fmt.Sprintf("%s spotted a fox sneaking toward the henhouse and chased it off.", a.Name), 2, 0}
	case "Tailor":
		f = flav{"🧵", fmt.Sprintf("%s stitched a ribbon of silk into her latest dress.", a.Name), 3, 0}
	default:
		return
	}
	a.Mood = clamp(a.Mood+f.mood, -100, 100)
	a.Money += f.money
	why := []string{fmt.Sprintf("%s at work", a.Occupation), "A village happening"}
	if f.money > 0 {
		why = append(why, "Found extra ore worth 10 gold")
	}
	w.addEvent(Event{Type: "flavor", Icon: f.icon, Actor: a.Name, Place: a.Place, Text: f.text, Why: why})
}

// ---- 决策（规则 Planner，Level 0） ----

// DecideFor 为某 Agent 决策下一个活动。
func (w *World) DecideFor(id int64) *Activity {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.agents[id]
	if a == nil {
		return nil
	}
	if a.Life != "" && !a.Life.IsActive() {
		return nil // 休眠/旅行/死亡/归档的 Agent 不参与决策（LifeState 守卫）
	}
	a.Due = false
	return w.decide(a)
}

func (w *World) decide(a *Agent) *Activity {
	hour := w.Minute() / 60
	now := w.now

	// 1) 睡觉时间
	if hour >= a.BedHour || hour < a.WakeHour {
		return &Activity{Kind: "sleep", Place: a.Home, At: now,
			Until: now + int64(((a.WakeHour-hour+24)%24)*60) + 1,
			Text:  "Sleeping soundly", Reason: []string{
				fmt.Sprintf("Bedtime is %02d:00", a.BedHour), "Sleep restores energy"}}
	}

	// 2) 跨世界出行（M9-A：Agent 自主决策，取代外部定时器扫描）
	// 铁匠在本地技艺未精时，自主决定去 Economy 世界谋生/精进；回村后技艺已提升便不再出发。
	if w.mover != nil && a.Occupation == "Blacksmith" && a.ActKind != "travel" {
		if top, lv := topSkill(a); top == "Blacksmithing" && lv < 80 {
			return &Activity{Kind: "travel", Place: "economy", Cross: true, At: now,
				Until: now + w.travelTime(a.Place, "economy"),
				Text:  "Setting out for the Economy world to seek work and skill",
				Reason: []string{
					fmt.Sprintf("My smithing is only Lv%d — there is still much to learn", lv),
					"The Economy world pays for a craftsman's skill",
				}}
		}
	}

	// 2) 执行玩家 Influence 计划（G0 §8-③：Agent 可以拒绝，接受了就真的去做）
	if a.Plan != nil {
		plan := a.Plan
		a.Plan = nil
		if plan.DueDay >= w.Day() {
			if plan.GoTo != "" && w.places[plan.GoTo] != nil {
				return &Activity{Kind: "social", Place: plan.GoTo, At: now, Until: now + 90,
					Text:   "Visiting the " + w.placeName(plan.GoTo),
					Reason: []string{"The Traveler suggested: " + plan.Text, "It sounded reasonable"}}
			}
			if plan.TalkTo != "" {
				if t := w.agentByName(plan.TalkTo); t != nil && t.ID != a.ID {
					return &Activity{Kind: "social", Place: t.Place, At: now, Until: now + 90,
						Text:   "Looking for " + t.Name,
						TalkTo: t.Name,
						Reason: []string{"The Traveler suggested a talk with " + t.Name, "Worth a try"}}
				}
			}
		}
	}

	// 3) Market 失火 → 铁匠去修
	if w.repair && a.Name == "John" && hour < a.BedHour-1 {
		return &Activity{Kind: "repair", Place: "market", At: now, Until: now + 120,
			Text:   "Repairing the fire-damaged Market stalls",
			Reason: []string{"The Market burned — my skills are needed", "Protecting local trade"}}
	}

	// 4) 精力不支 → 回家休息
	if a.Energy < 22 {
		return &Activity{Kind: "rest", Place: a.Home, At: now, Until: now + 180,
			Text: "Resting at home", Reason: []string{
				fmt.Sprintf("Energy is low (%d/100)", a.Energy), "A rest restores the body"}}
	}

	// 5) 饭点
	if (hour == 12 || hour == 18) && a.ActKind != "eat" && a.Money >= 8 {
		return &Activity{Kind: "eat", Place: "tavern", At: now, Until: now + 45, Spend: 6,
			Text:   "Having a hot meal at the Tavern",
			Reason: []string{"Mealtime", "Hot food restores energy and mood"}}
	}

	// 6) 工作时间
	if hour >= a.WakeHour && hour < a.BedHour-1 {
		p := 0.62 + (a.Grit-0.5)*0.2
		if a.Money < 15 {
			p += 0.25
		}
		if a.GoalDone || a.GoalAbandoned {
			p -= 0.25
		}
		if a.Mood < -30 {
			p -= 0.1
		}
		if w.rng.Float64() < p {
			reason := []string{fmt.Sprintf("Work hours (%02d:00)", hour)}
			if a.GoalTarget > 0 && !a.GoalDone && !a.GoalAbandoned {
				reason = append(reason, fmt.Sprintf("Saving for \"%s\" (%d/%d gold)", a.Goal, a.Money, a.GoalTarget))
			} else if a.GoalTarget == 0 || a.GoalAbandoned {
				reason = append(reason, "Duty and pride in the craft")
			}
			if a.Money < 15 {
				reason = append(reason, "The pouch is nearly empty")
			}
			if top, lv := topSkill(a); top != "" {
				reason = append(reason, fmt.Sprintf("Skill: %s %d", top, lv))
			}
			return &Activity{Kind: "work", Place: a.Workplace, At: now,
				Until: now + 150 + int64(w.rng.Intn(90)),
				Text:  workText(a), Reason: reason}
		}
	}

	// 7) 傍晚社交
	if hour >= 17 && hour < a.BedHour-1 {
		if a.Name == "Grace" && hour >= 19 {
			return &Activity{Kind: "social", Place: "church", At: now, Until: now + 90,
				Text:   "Holding evening service at the Church",
				Reason: []string{"Evening prayer", "The village needs its rituals"}}
		}
		p := 0.30 + a.Social*0.45
		if a.Mood < -20 {
			p += 0.15
		}
		if w.rng.Float64() < p {
			return &Activity{Kind: "social", Place: "tavern", At: now, Until: now + 90,
				Text:   "Sharing gossip and ale at the Tavern",
				Reason: []string{"Evening is for company", fmt.Sprintf("Sociable by nature (%d%%)", int(a.Social*100))}}
		}
	}

	// 8) 逛街购物
	if w.rng.Float64() < 0.12 && a.Money >= 15 {
		return &Activity{Kind: "shop", Place: "market", At: now, Until: now + 60, Spend: 10,
			Text:   "Browsing the Market stalls",
			Reason: []string{"A little shopping lifts the spirits", "Some small comforts"}}
	}

	// 9) 默认：广场散步
	return &Activity{Kind: "idle", Place: "town_square", At: now, Until: now + 60,
		Text:   "Strolling around the Town Square",
		Reason: []string{"Nothing pressing at the moment", "A walk clears the head"}}
}

// ApplyDecision 执行决策（模块 Executor 调用）。返回给人看的结果描述。
func (w *World) ApplyDecision(id int64, act *Activity) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.agents[id]
	if a == nil || act == nil {
		return ""
	}
	// 正在做的事没变（同地同活动）→ 静默延长，不刷事件
	if a.ActKind == act.Kind && a.Place == act.Place && a.ActKind != "travel" {
		a.ActionUntil = act.Until
		return ""
	}
	if a.Place != act.Place {
		// 出发：先走，到达后（Tick）再 applyAct
		a.ActKind = "travel"
		a.TravelTo = act.Place
		a.Crossing = act.Cross
		a.NextAct = act
		a.Action = "Walking to the " + w.placeName(act.Place)
		a.ActionUntil = w.now + w.travelTime(a.Place, act.Place)
		w.addEvent(Event{Type: "move", Icon: "🚶", Actor: a.Name, Place: a.Place,
			Text: fmt.Sprintf("%s set off toward the %s.", a.Name, w.placeName(act.Place)),
			Why:  act.Reason})
		return a.Action
	}
	w.applyAct(a, act, false)
	return a.Action
}

// applyAct 真正进入活动（已在目标地点）。
func (w *World) applyAct(a *Agent, act *Activity, arrived bool) {
	if act.Spend > 0 && a.Money >= act.Spend {
		a.Money -= act.Spend
	}
	a.ActKind = act.Kind
	a.Action = act.Text
	a.ActionUntil = act.Until
	a.TravelTo = ""
	a.Crossing = false
	a.NextAct = nil
	a.Due = false
	verb := map[string]string{
		"work": "⚒️", "sleep": "😴", "eat": "🍲", "social": "🗣️",
		"rest": "🛏️", "shop": "🛍️", "idle": "🚶", "repair": "🔨",
	}
	if arrived {
		w.addEvent(Event{Type: "activity", Icon: verb[act.Kind], Actor: a.Name, Place: a.Place,
			Text: fmt.Sprintf("%s — %s", a.Name, act.Text), Why: act.Reason})
	}
}

// ---- 玩家动作（G0 §8） ----

// Chat 玩家与 Agent 对话。对话进入记忆、提升关系、产生事件。
func (w *World) Chat(id int64, msg string) map[string]interface{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.agents[id]
	if a == nil || a.Gone {
		return nil
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil
	}
	// 意图识别：玩家向村民要金币。文本是表达，Action 才是事实——
	// 命中则直接走 Borrow 动作（真实移动金币），绝不把 LLM 的客套话当事实。
	if amt, ok := parseBorrowIntent(msg); ok {
		return w.borrowLocked(id, amt)
	}
	q, ok := w.spendActLocked()
	if !ok {
		return actDenied(q)
	}
	reply := w.replyFor(a, msg)
	a.Mood = clamp(a.Mood+2, -100, 100)
	pr := playerRel(a)
	pr.Like = clamp(pr.Like+2, 0, 100)
	pr.Trust = clamp(pr.Trust+1, 0, 100)
	w.rememberPlayer(a, "The Traveler asked me: "+trunc(msg, 80), 2, "chat")
	w.remember(a, "I answered: "+trunc(reply, 80), 1)
	w.player.Chats++
	w.addEvent(Event{Type: "player", Icon: "💬", Actor: "The Traveler", Target: a.Name, Place: a.Place,
		Text: fmt.Sprintf("The Traveler had a word with %s.", a.Name),
		Why:  []string{"Player interaction", fmt.Sprintf("%s's mood: %s", a.Name, MoodEmoji(a.Mood))}})
	return map[string]interface{}{
		"reply": reply, "like": pr.Like, "trust": pr.Trust,
		"money": a.Money, "mood": MoodEmoji(a.Mood), "agent": w.brief(a), "quota": q,
	}
}

// Influence 玩家给建议；Agent 考虑后接受或拒绝（玩家不是上帝，G0 §8-③）。
func (w *World) Influence(id int64, advice string) map[string]interface{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.agents[id]
	if a == nil || a.Gone {
		return nil
	}
	advice = strings.TrimSpace(advice)
	if advice == "" {
		return nil
	}
	q, ok := w.spendActLocked()
	if !ok {
		return actDenied(q)
	}
	goTo, talkTo := w.parseAdvice(advice)
	pr := playerRel(a)
	p := 0.35 + float64(pr.Like)/300.0 + float64(pr.Trust)/300.0 - a.Grit*0.25
	if goTo == a.Workplace {
		p += 0.15
	}
	if p < 0.08 {
		p = 0.08
	}
	if p > 0.92 {
		p = 0.92
	}
	accepted := w.rng.Float64() < p
	var reply string
	infStrength := int(p * 100)
	if accepted {
		a.Plan = &Plan{Text: advice, GoTo: goTo, TalkTo: talkTo, DueDay: w.Day() + 1}
		pr.Like = clamp(pr.Like+3, 0, 100)
		pr.Trust = clamp(pr.Trust+1, 0, 100)
		reply = pickStr(w.rng, []string{
			"Hm... that's not a bad idea. I'll give it a try.",
			"You think so? Well... alright, I'll consider it.",
			"Coming from you, I'll trust that. Let's see.",
		})
		tr := &WhyTrace{
			Rule:     "influence.accept",
			Cause:    []string{fmt.Sprintf("trust=%d", pr.Trust), fmt.Sprintf("influence_strength=%d", infStrength), fmt.Sprintf("grit=%d", int(a.Grit*100))},
			Recent:   recentPlayerMemories(a, 2),
			Decision: "accepted player's influence",
		}
		w.rememberPlayer(a, "The Traveler suggested: "+trunc(advice, 80)+". I said I'd try.", 3, "advice")
		w.addEvent(Event{Type: "player", Icon: "✅", Actor: "The Traveler", Target: a.Name, Place: a.Place,
			Text: fmt.Sprintf("The Traveler advised %s: \"%s\" — %s agreed to consider it.", a.Name, trunc(advice, 60), a.Name),
			Why:  []string{"Player influence", fmt.Sprintf("Accept chance %.0f%%", p*100)},
			Trace: tr})
		w.logWhy(a.Name, "influence.accept", tr, trunc(advice, 60))
	} else {
		pr.Like = clamp(pr.Like-1, 0, 100)
		reply = w.rejectLine(a)
		tr := &WhyTrace{
			Rule:     "influence.reject",
			Cause:    []string{fmt.Sprintf("trust=%d", pr.Trust), fmt.Sprintf("influence_strength=%d", infStrength), fmt.Sprintf("grit=%d", int(a.Grit*100))},
			Recent:   recentPlayerMemories(a, 2),
			Decision: "rejected player's influence (grit too high / trust too low)",
		}
		w.rememberPlayer(a, "The Traveler suggested: "+trunc(advice, 80)+". I said no.", 2, "refuse")
		w.addEvent(Event{Type: "player", Icon: "❌", Actor: "The Traveler", Target: a.Name, Place: a.Place,
			Text: fmt.Sprintf("The Traveler advised %s: \"%s\" — %s refused.", a.Name, trunc(advice, 60), a.Name),
			Why:  []string{"Player influence rejected", fmt.Sprintf("%s is stubborn (%d%% grit)", a.Name, int(a.Grit*100))},
			Trace: tr})
		w.logWhy(a.Name, "influence.reject", tr, trunc(advice, 60))
	}
	return map[string]interface{}{
		"accepted": accepted, "reply": reply, "probability": p,
		"agent": w.brief(a), "quota": q,
	}
}

// Gift 玩家送金币。
func (w *World) Gift(id int64, amount int64) map[string]interface{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.agents[id]
	if a == nil || a.Gone || amount <= 0 || w.player.Gold < amount {
		return nil
	}
	q, ok := w.spendActLocked()
	if !ok {
		return actDenied(q)
	}
	w.player.Gold -= amount
	w.player.Given += amount
	a.Money += amount
	pr := playerRel(a)
	pr.Like = clamp(pr.Like+int(amount/10)+2, 0, 100)
	if amount >= 50 {
		pr.Trust = clamp(pr.Trust+2, 0, 100)
	}
	a.Mood = clamp(a.Mood+10, -100, 100)
	w.rememberPlayer(a, fmt.Sprintf("The Traveler gave me %d gold. I won't forget this kindness.", amount), 4, "gift")
	goalLine := ""
	if a.GoalTarget > 0 && !a.GoalDone && !a.GoalAbandoned {
		goalLine = fmt.Sprintf(" Now %d/%d toward \"%s\".", a.Money, a.GoalTarget, a.Goal)
	} else if a.GoalAbandoned {
		goalLine = " (He has given up on his dream, though.)"
	}
	w.addEvent(Event{Type: "player", Icon: "🎁", Actor: "The Traveler", Target: a.Name, Place: a.Place,
		Text: fmt.Sprintf("The Traveler gave %s %d gold.%s", a.Name, amount, goalLine),
		Why:  []string{"Player generosity", fmt.Sprintf("Relationship (Like) now %d", pr.Like)}})
	reply := pickStr(w.rng, []string{
		"Thank you, Traveler. This really helps.",
		"Bless you! I'll put this to good use.",
		"I... thank you. Truly.",
	})
	return map[string]interface{}{
		"reply": reply, "money": a.Money, "like": pr.Like, "trust": pr.Trust,
		"player_gold": w.player.Gold, "agent": w.brief(a), "quota": q,
	}
}

// Borrow 玩家向村民讨/借金币（不记债务）。公开入口。
func (w *World) Borrow(id int64, amount int64) map[string]interface{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.borrowLocked(id, amount)
}

// borrowLocked 真实移动金币：村民把金币转给玩家。须持锁调用。
// 这是"事实"所在——LLM 文本里说的"我给你"不在此生效。
func (w *World) borrowLocked(id int64, amount int64) map[string]interface{} {
	a := w.agents[id]
	if a == nil || a.Gone {
		return nil
	}
	if amount <= 0 {
		return nil
	}
	q, ok := w.spendActLocked()
	if !ok {
		return actDenied(q)
	}
	give := amount
	if give > a.Money {
		give = a.Money // 村民最多给出他身上所有的
	}
	if give <= 0 {
		w.remember(a, "The Traveler asked me for gold, but my purse was empty.", 2)
		w.addEvent(Event{Type: "player", Icon: "🪙", Actor: "The Traveler", Target: a.Name, Place: a.Place,
			Text: fmt.Sprintf("The Traveler asked %s for gold, but %s had none to spare.", a.Name, a.Name),
			Why:  []string{"Player request", fmt.Sprintf("%s's purse is empty", a.Name)}})
		reply := pickStr(w.rng, []string{
			"I wish I could, Traveler, but my purse is empty.",
			"No coin to my name today — sorry.",
		})
		return map[string]interface{}{
			"reply": reply, "money": a.Money, "player_gold": w.player.Gold,
			"agent": w.brief(a), "quota": q,
		}
	}
	a.Money -= give
	w.player.Gold += give
	w.player.Borrowed += give
	pr := playerRel(a)
	pr.Like = clamp(pr.Like+2, 0, 100)
	pr.Trust = clamp(pr.Trust+1, 0, 100)
	a.Mood = clamp(a.Mood+2, -100, 100)
	w.rememberPlayer(a, fmt.Sprintf("The Traveler asked me for %d gold and I gave it. He needed it more than my purse did.", give), 3, "borrow")
	w.addEvent(Event{Type: "player", Icon: "🤝", Actor: "The Traveler", Target: a.Name, Place: a.Place,
		Text: fmt.Sprintf("%s gave the Traveler %d gold.", a.Name, give),
		Why:  []string{"Player requested it", fmt.Sprintf("%s had %d gold before", a.Name, a.Money+give)}})
	reply := pickStr(w.rng, []string{
		fmt.Sprintf("Here, Traveler — %d gold. May it serve you well.", give),
		fmt.Sprintf("Take %d gold. We look out for each other here.", give),
	})
	return map[string]interface{}{
		"reply": reply, "money": a.Money, "player_gold": w.player.Gold,
		"like": pr.Like, "trust": pr.Trust, "agent": w.brief(a), "quota": q,
	}
}

// parseBorrowIntent 识别"玩家向村民要金币"的意图。命中返回金额。
// 关键词须是"玩家接收"语义（借我/给我/讨/要），并带有金币意图或数字，
// 以免"给我讲个故事"这类表达误触。
func parseBorrowIntent(msg string) (int64, bool) {
	receive := false
	for _, k := range []string{"借我", "借点", "给我", "给我点", "讨", "要"} {
		if strings.Contains(msg, k) {
			receive = true
			break
		}
	}
	if !receive {
		return 0, false
	}
	hasGold := strings.Contains(msg, "金币") || strings.Contains(msg, "金子") ||
		strings.Contains(msg, "gold") || strings.Contains(msg, "coin") ||
		strings.Contains(msg, "钱") || strings.Contains(msg, "币")
	amt := extractGoldAmount(msg)
	if !hasGold && amt <= 0 {
		return 0, false
	}
	if amt <= 0 {
		amt = 10 // "给我点金币" → 默认 10
	}
	return amt, true
}

// extractGoldAmount 从文本提取金币数：优先数字，其次单字中文数字。
func extractGoldAmount(msg string) int64 {
	if m := goldNumRe.FindStringSubmatch(msg); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return n
		}
	}
	for _, r := range msg {
		if v, ok := cnNum[r]; ok {
			return v
		}
	}
	return 0
}

var goldNumRe = regexp.MustCompile(`(\d+)`)
var cnNum = map[rune]int64{'一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9, '十': 10}

// replyFor 生成回复：配置了 LLM 走 Level 2，否则走模板。
func (w *World) replyFor(a *Agent, msg string) string {
	if w.chatLLM != nil && w.chatLLM.Enabled() {
		reply, err := w.chatLLM.Chat(context.Background(), w.chatSystem(a), msg)
		if err != nil {
			log.Printf("[village] 玩家对话 LLM 调用失败（回退模板）: %v", err)
		} else if strings.TrimSpace(reply) != "" {
			return strings.TrimSpace(reply)
		}
	}
	return w.templateReply(a, msg)
}

func (w *World) chatSystem(a *Agent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, the %s of Willow Creek village. Personality: %s.\n",
		a.Name, a.Occupation, strings.Join(a.Personality, ", "))
	fmt.Fprintf(&b, "Gold: %d. Energy: %d/100. Mood: %s. Currently: %s at the %s.\n",
		a.Money, a.Energy, MoodEmoji(a.Mood), strings.ToLower(a.Action), w.placeName(a.Place))
	if a.GoalTarget > 0 {
		if a.GoalAbandoned {
			fmt.Fprintf(&b, "Dream: %s — ABANDONED (saved %d/%d gold before giving up).\n", a.Goal, a.Money, a.GoalTarget)
		} else {
			fmt.Fprintf(&b, "Dream: %s (saved %d/%d gold).\n", a.Goal, a.Money, a.GoalTarget)
		}
	} else {
		fmt.Fprintf(&b, "Ambition: %s\n", a.Goal)
	}
	if names := w.relSummary(a, 3); names != "" {
		b.WriteString("Relationships: " + names + "\n")
	}
	if mems := w.recentMemories(a, 5); mems != "" {
		b.WriteString("Recent memories:\n" + mems + "\n")
	}
	b.WriteString("Reply in character, at most 3 short sentences, first person. Speak like a medieval villager. Match the language the human uses.")
	return b.String()
}

// templateReply 关键词模板回复（无 LLM 时）。
func (w *World) templateReply(a *Agent, msg string) string {
	lower := strings.ToLower(msg)
	// 提到其他村民 → 用关系回答
	for _, oid := range w.agentOrd {
		o := w.agents[oid]
		if o.ID == a.ID || !strings.Contains(lower, strings.ToLower(o.Name)) {
			continue
		}
		rel := a.Rel[o.ID]
		if rel == nil {
			rel = &Relationship{Like: 40, Trust: 40}
		}
		if amt, ok := a.Owes[o.ID]; ok && amt > 0 {
			return fmt.Sprintf("%s? I... I still owe them %d gold. I'll pay it back, I swear.", o.Name, amt)
		}
		if rel.Like >= 60 {
			return fmt.Sprintf("%s? One of the good ones. I'd trust them with my last coin.", o.Name)
		}
		if rel.Like <= 25 {
			return fmt.Sprintf("%s... Hmph. Let's just say we don't see eye to eye.", o.Name)
		}
		return fmt.Sprintf("%s is alright. We cross paths now and then.", o.Name)
	}
	switch {
	case containsAny(lower, "money", "gold", "rich", "coin"):
		if a.GoalTarget > 0 && !a.GoalAbandoned {
			return fmt.Sprintf("I've got %d gold. %d more and my dream of %s comes true.", a.Money, max64(0, a.GoalTarget-a.Money), lowerFirst(a.Goal))
		}
		if a.GoalAbandoned {
			return fmt.Sprintf("Coin comes and goes. Right now I'm holding %d gold — and no dream worth saving for.", a.Money)
		}
		return fmt.Sprintf("Coin comes and goes. Right now I'm holding %d gold.", a.Money)
	case containsAny(lower, "dream", "goal", "shop", "ambition", "future", "want"):
		if a.GoalTarget > 0 && !a.GoalAbandoned {
			return fmt.Sprintf("%s. I've saved %d of %d gold so far. Every day counts.", a.Goal, a.Money, a.GoalTarget)
		}
		if a.GoalAbandoned {
			return fmt.Sprintf("%s used to be my dream. I've set it aside. Don't remind me.", a.Goal)
		}
		return a.Goal + ". That's what I'm working toward."
	case containsAny(lower, "work", "job", "craft", "trade"):
		return fmt.Sprintf("I'm a %s — %s", strings.ToLower(a.Occupation), a.Lines[w.rng.Intn(len(a.Lines))])
	case containsAny(lower, "hello", "hi", "hey", "morning", "evening", "你好", "嗨"):
		if a.Mood >= 30 {
			return "Well met, Traveler! The day treats me kindly so far."
		}
		if a.Mood >= -15 {
			return "Hello there. Fine day for a walk, I suppose."
		}
		return "Hm? Oh... hello. My mind was elsewhere."
	case containsAny(lower, "how are you", "mood", "feel"):
		return fmt.Sprintf("I'm feeling %s these days. %s", strings.ToLower(moodWord(a.Mood)), a.Lines[w.rng.Intn(len(a.Lines))])
	}
	return a.Lines[w.rng.Intn(len(a.Lines))]
}

func (w *World) rejectLine(a *Agent) string {
	if hasTrait(a, "Proud", "Stubborn") {
		return "Hmph! I run my own life, Traveler. No offense."
	}
	if hasTrait(a, "Shy", "Quiet") {
		return "...I'd rather not. Sorry."
	}
	return pickStr(w.rng, []string{
		"Maybe later. I'm busy right now.",
		"I don't think that's for me, friend.",
		"Kind of you, but no. I have my own plan.",
	})
}

// ---- 状态视图 ----

func (w *World) stateLocked() map[string]interface{} {
	places := make([]map[string]interface{}, 0, len(w.placeOrd))
	edges := [][2]string{}
	for _, pid := range w.placeOrd {
		p := w.places[pid]
		agents := []AgentBrief{}
		for _, id := range w.agentOrd {
			a := w.agents[id]
			if a.Place == pid && !a.Gone {
				agents = append(agents, w.brief(a))
			}
		}
		places = append(places, map[string]interface{}{
			"id": p.ID, "name": p.Name, "emoji": p.Emoji, "desc": p.Desc,
			"x": p.X, "y": p.Y, "agents": agents,
		})
		for _, l := range p.Links {
			if l > p.ID {
				edges = append(edges, [2]string{p.ID, l})
			}
		}
	}
	agents := make([]AgentBrief, 0, len(w.agentOrd))
	for _, id := range w.agentOrd {
		agents = append(agents, w.brief(w.agents[id]))
	}
	return map[string]interface{}{
		"village": map[string]interface{}{
			"name": VillageName, "day": w.Day(), "minute": w.Minute(),
			"clock": w.Clock(), "weather": w.weather, "speed": w.speed,
			"llm": w.chatLLM != nil && w.chatLLM.Enabled(),
		},
		"places": places, "edges": edges, "agents": agents, "player": w.player,
		"threads": w.threadViewsLocked(), "echoes": w.echoFeedLocked(6),
		"quota": w.quotaLocked(),
	}
}

// State 全量状态快照（REST 与 SSE 共用）。
func (w *World) State() map[string]interface{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stateLocked()
}

// AgentDetail 单个 Agent 深度视图（Inspector）。
func (w *World) AgentDetail(id int64) map[string]interface{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.agents[id]
	if a == nil {
		return nil
	}
	type relView struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Like     int    `json:"like"`
		Trust    int    `json:"trust"`
		IsPlayer bool   `json:"is_player"`
	}
	rels := []relView{}
	for oid, r := range a.Rel {
		rels = append(rels, relView{ID: oid, Name: w.nameOf(oid), Like: r.Like, Trust: r.Trust, IsPlayer: oid == 0})
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].Like > rels[j].Like })
	memCount := 12
	if len(a.Mem) < memCount {
		memCount = len(a.Mem)
	}
	mems := make([]Memory, memCount)
	copy(mems, a.Mem[:memCount])
	evs := []Event{}
	for _, e := range w.events {
		if e.Actor == a.Name || e.Target == a.Name {
			evs = append(evs, e)
			if len(evs) >= 20 {
				break
			}
		}
	}
	skills := map[string]int{}
	for k, v := range a.Skills {
		skills[k] = v
	}
	return map[string]interface{}{
		"agent":         w.brief(a),
		"personality":   a.Personality,
		"skills":        skills,
		"place_name":    w.placeName(a.Place),
		"goal_target":   a.GoalTarget,
		"relationships": rels,
		"memories":      mems,
		"recent_events": evs,
		"owes":          w.owesView(a),
	}
}

func (w *World) owesView(a *Agent) []map[string]interface{} {
	out := []map[string]interface{}{}
	for oid, amt := range a.Owes {
		if amt > 0 {
			out = append(out, map[string]interface{}{"to": w.nameOf(oid), "amount": amt})
		}
	}
	return out
}

// IsDue 唤醒策略用：该 Agent 是否需要决策。
func (w *World) IsDue(id int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.agents[id]
	return a != nil && !a.Gone && (a.Due || a.ActionUntil <= w.now)
}

// ---- 小工具 ----

func (w *World) pickWeather() string {
	return pickStr(w.rng, []string{"Sunny", "Sunny", "Cloudy", "Rainy", "Windy"})
}

func (w *World) placeName(id string) string {
	if p := w.places[id]; p != nil {
		return p.Name
	}
	return id
}

func (w *World) nameOf(id int64) string {
	if id == 0 {
		return "the Traveler"
	}
	if a := w.agents[id]; a != nil {
		return a.Name
	}
	return fmt.Sprintf("Villager#%d", id)
}

func (w *World) agentByName(name string) *Agent {
	for _, id := range w.agentOrd {
		if w.agents[id].Name == name {
			return w.agents[id]
		}
	}
	return nil
}

func (w *World) debtBetween(a, b *Agent) (*Agent, *Agent, int64) {
	if amt := a.Owes[b.ID]; amt > 0 {
		return b, a, amt // b 是债主
	}
	if amt := b.Owes[a.ID]; amt > 0 {
		return a, b, amt
	}
	return nil, nil, 0
}

func (w *World) adjustRel(from, to *Agent, dLike, dTrust int) {
	r := from.Rel[to.ID]
	if r == nil {
		r = &Relationship{Like: 40, Trust: 40}
		from.Rel[to.ID] = r
	}
	r.Like = clamp(r.Like+dLike, 0, 100)
	r.Trust = clamp(r.Trust+dTrust, 0, 100)
}

// remember 写入一条记忆。对"非玩家痕迹"的记忆做近期去重，避免同一反思
// 被每天重复堆叠（scarcity 反思、交租失败等硬编码文本都会命中）。
// 玩家痕迹（src=player）不去重，以保证 echo 回响机制依赖的 a.Mem[0] 始终是最新一条。
func (w *World) remember(a *Agent, text string, imp int) {
	const dedupeWindow = 30
	for i, m := range a.Mem {
		if i >= dedupeWindow {
			break
		}
		if m.Src != "player" && m.Text == text {
			return
		}
	}
	a.Mem = append([]Memory{{Day: w.Day(), Minute: w.Minute(), Text: text, Imp: imp}}, a.Mem...)
	if len(a.Mem) > 60 {
		a.Mem = a.Mem[:60]
	}
}

func (w *World) relSummary(a *Agent, n int) string {
	type r struct {
		name string
		like int
	}
	var rs []r
	for oid, rel := range a.Rel {
		if oid == 0 {
			continue
		}
		rs = append(rs, r{w.nameOf(oid), rel.Like})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].like > rs[j].like })
	parts := []string{}
	for i, x := range rs {
		if i >= n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s(%d/100)", x.name, x.like))
	}
	return strings.Join(parts, ", ")
}

func (w *World) recentMemories(a *Agent, n int) string {
	parts := []string{}
	for i, m := range a.Mem {
		if i >= n {
			break
		}
		parts = append(parts, "- "+m.Text)
	}
	return strings.Join(parts, "\n")
}

func (w *World) wageFor(a *Agent) int64 {
	base := map[string]int64{
		"Blacksmith": 22, "Innkeeper": 18, "Merchant": 26, "Farmer": 16,
		"Healer": 14, "Baker": 18, "Miner": 24, "Priest": 8, "Guard": 15, "Tailor": 17,
	}[a.Occupation]
	if base == 0 {
		base = 12
	}
	_, lv := topSkill(a)
	wage := int64(float64(base) * (1 + float64(lv)/200.0))
	return wage + int64(w.rng.Intn(5))
}

func (w *World) travelTime(from, to string) int64 {
	d := w.dist(from, to)
	return int64(d)*12 + int64(w.rng.Intn(8))
}

// dist BFS 最短跳数（初始化时算好）。
func (w *World) dist(from, to string) int {
	if from == to {
		return 0
	}
	type node struct {
		id string
		d  int
	}
	seen := map[string]bool{from: true}
	q := []node{{from, 0}}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		p := w.places[cur.id]
		if p == nil {
			continue
		}
		for _, l := range p.Links {
			if l == to {
				return cur.d + 1
			}
			if !seen[l] {
				seen[l] = true
				q = append(q, node{l, cur.d + 1})
			}
		}
	}
	return 3
}

// parseAdvice 从建议里解析目标地点/人物。
func (w *World) parseAdvice(advice string) (string, string) {
	lower := strings.ToLower(advice)
	placeWords := map[string]string{
		"tavern": "tavern", "market": "market", "mine": "mine", "farm": "farm",
		"church": "church", "square": "town_square", "castle": "castle",
		"home": "residential", "house": "residential", "residential": "residential",
	}
	for word, pid := range placeWords {
		if strings.Contains(lower, word) {
			return pid, ""
		}
	}
	if idx := strings.Index(lower, "talk to "); idx >= 0 {
		rest := strings.TrimSpace(advice[idx+8:])
		for _, id := range w.agentOrd {
			name := w.agents[id].Name
			if strings.HasPrefix(strings.ToLower(rest), strings.ToLower(name)) {
				return "", name
			}
			if strings.Contains(lower, strings.ToLower(name)) {
				return "", name
			}
		}
	}
	for _, id := range w.agentOrd {
		if strings.Contains(lower, strings.ToLower(w.agents[id].Name)) {
			return "", w.agents[id].Name
		}
	}
	return "", ""
}

// workText 职业工作动作文案。
func workText(a *Agent) string {
	switch a.Occupation {
	case "Blacksmith":
		return "Working the forge at the Mine"
	case "Innkeeper":
		return "Serving ale and stew at the Tavern"
	case "Merchant":
		return "Minding the stall at the Market"
	case "Farmer":
		return "Tending the wheat fields"
	case "Healer":
		return "Preparing remedies at the Church"
	case "Baker":
		return "Baking bread for the morning"
	case "Miner":
		return "Swinging the pickaxe in the Mine"
	case "Priest":
		return "Tending the church and the poor"
	case "Guard":
		return "Standing watch at the Castle"
	case "Tailor":
		return "Stitching at the workshop"
	}
	return "Working"
}

func topSkill(a *Agent) (string, int) {
	best, lv := "", 0
	for k, v := range a.Skills {
		if v > lv {
			best, lv = k, v
		}
	}
	return best, lv
}

func playerRel(a *Agent) *Relationship {
	r := a.Rel[0]
	if r == nil {
		r = &Relationship{Like: 20, Trust: 20}
		a.Rel[0] = r
	}
	return r
}

func hasTrait(a *Agent, traits ...string) bool {
	for _, t := range a.Personality {
		for _, want := range traits {
			if t == want {
				return true
			}
		}
	}
	return false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func moodWord(m int) string {
	switch {
	case m >= 60:
		return "Cheerful"
	case m >= 30:
		return "Happy"
	case m >= 5:
		return "Content"
	case m >= -15:
		return "Fine"
	case m >= -40:
		return "Troubled"
	default:
		return "Furious"
	}
}

func pickStr(r *rand.Rand, opts []string) string { return opts[r.Intn(len(opts))] }

func trunc(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
