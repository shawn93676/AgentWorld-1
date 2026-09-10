package village

import "fmt"

// echo.go —— 长程因果：玩家的干预在数天之后回响。
//
// 为什么需要这一层：村庄原本只有"当下"——你给的钱、你提的建议，当次事件一过
// 就沉进日志，玩家看不出自己改变过什么。没有"我改变了这个世界"的证据，就留不住人。
//
// 机制：
//   - 玩家的任何干预（赠礼 / 建议 / 对话 / 悬念中的选择）都写一条 src=player 的记忆，
//     并在 2~5 天后约定一个回响日；
//   - 到那天，世界把这件事重新提起，产生一条 type=echo 的事件，Why 里引用原始记忆
//     （正好接上 G0 §11 的"Why did this happen?"）；
//   - 一半概率这件事会被说出去：另一名村民听说后，对玩家的好感/信任上升。
//     玩家的痕迹由此扩散到他从未接触过的人身上。

// echoDelay 回响的延迟范围（天）。
const echoDelayMin = 2
const echoDelayMax = 5

// rememberPlayer 记一条"玩家留下的痕迹"，并约定回响日。
func (w *World) rememberPlayer(a *Agent, text string, imp int, tag string) {
	w.remember(a, text, imp)
	if len(a.Mem) == 0 {
		return
	}
	m := &a.Mem[0]
	m.Src = "player"
	m.Tag = tag
	m.EchoDay = w.Day() + echoDelayMin + w.rng.Intn(echoDelayMax-echoDelayMin+1)
}

// runEchoesLocked 检查并触发到期的回响。须持锁调用。
func (w *World) runEchoesLocked() {
	today := w.Day()
	for _, id := range w.agentOrd {
		a := w.agents[id]
		if a == nil {
			continue
		}
		for i := range a.Mem {
			m := &a.Mem[i]
			if m.Src != "player" || m.Echoed || m.EchoDay == 0 || today < m.EchoDay {
				continue
			}
			m.Echoed = true
			w.echoPlayerTrace(a, m)
		}
	}
}

// echoPlayerTrace 让一条玩家痕迹在世界里回响。
func (w *World) echoPlayerTrace(a *Agent, m *Memory) {
	text := w.echoText(a, m)
	w.addEvent(Event{Type: "echo", Icon: "🪶", Actor: a.Name, Place: a.Place, Text: text,
		Why: []string{"Something the Traveler did, days ago",
			fmt.Sprintf("Remembered from day %d: \"%s\"", m.Day, trunc(m.Text, 70))}})

	// 只有"善意"会传播；威胁、拒绝、袖手旁观留在原地。
	if !positiveTrace[m.Tag] {
		return
	}
	if w.rng.Float64() >= 0.5 {
		return
	}
	b := w.closestTo(a)
	if b == nil {
		return
	}
	pr := playerRel(b)
	pr.Like = clamp(pr.Like+3, 0, 100)
	pr.Trust = clamp(pr.Trust+2, 0, 100)
	w.remember(b, fmt.Sprintf("Word got around: the Traveler helped %s when it mattered.", a.Name), 2)
	w.addEvent(Event{Type: "echo", Icon: "🪶", Actor: b.Name, Place: b.Place,
		Text: fmt.Sprintf("%s heard what the Traveler did for %s. It changes how he sees you.", b.Name, a.Name),
		Why:  []string{"Villagers talk", fmt.Sprintf("%s is close to %s", b.Name, a.Name)}})
}

// positiveTrace 哪些干预类型是善意的（会被说出去）。
var positiveTrace = map[string]bool{
	"gift": true, "fund_give": true, "fund_work": true,
	"debt_pay": true, "mediate": true, "sided_with": true,
	"advice": true, "omen_ask": true, "borrow": true,
}

// echoText 按干预类型生成回响台词。
func (w *World) echoText(a *Agent, m *Memory) string {
	switch m.Tag {
	case "gift", "fund_give":
		if a.GoalDone {
			return fmt.Sprintf("%s's dream finally came true. Ask him who made it possible and he names a traveler.", a.Name)
		}
		if a.GoalAbandoned {
			return fmt.Sprintf("%s still keeps the coins the Traveler gave him, but his old dream is set aside now. He doesn't talk about it.", a.Name)
		}
		return pickStr(w.rng, []string{
			fmt.Sprintf("%s still keeps the coins the Traveler gave him apart from the rest. They are not for spending.", a.Name),
			fmt.Sprintf("Tonight %s counted his purse twice, and thought of the hand that filled it.", a.Name),
			fmt.Sprintf("%s mentioned the Traveler's gift again today. He does that when he is worried about money.", a.Name),
		})
	case "fund_work":
		return fmt.Sprintf("%s's hands are rougher than they were. The extra work the Traveler found him is still paying.", a.Name)
	case "debt_pay":
		return fmt.Sprintf("The debt is gone, but %s has not forgotten who carried it. He tells anyone who asks.", a.Name)
	case "debt_press":
		return fmt.Sprintf("%s paid what he owed — but he still goes quiet when the Traveler's name comes up.", a.Name)
	case "mediate":
		return fmt.Sprintf("%s sat on a bench beside an old grudge today, and it was almost comfortable.", a.Name)
	case "sided_with":
		return fmt.Sprintf("%s still thinks the Traveler chose rightly. He says so when the other one isn't there.", a.Name)
	case "side":
		return fmt.Sprintf("%s has not forgiven the Traveler for taking the other side, and he makes it obvious.", a.Name)
	case "advice":
		return fmt.Sprintf("%s still does it the way the Traveler suggested. It has become habit now.", a.Name)
	case "refuse":
		return fmt.Sprintf("%s remembered the advice he refused, and was quietly glad he said no.", a.Name)
	case "omen_ask":
		return fmt.Sprintf("The small thing %s told the Traveler has grown into a story the village repeats.", a.Name)
	default: // chat
		return fmt.Sprintf("%s repeated something the Traveler once said to him. It sounded wiser coming from him.", a.Name)
	}
}

// closestTo 与 a 最亲近的村民（用于传播玩家的痕迹）。
func (w *World) closestTo(a *Agent) *Agent {
	var best *Agent
	bestLike := -1
	for oid, r := range a.Rel {
		if oid == 0 {
			continue
		}
		o := w.agents[oid]
		if o == nil || o.ID == a.ID || o.Gone {
			continue
		}
		if r.Like > bestLike {
			bestLike, best = r.Like, o
		}
	}
	return best
}

// echoFeedLocked 最近 n 条回响事件（新的在前），供前端"你的痕迹"使用。
func (w *World) echoFeedLocked(n int) []Event {
	out := []Event{}
	for _, e := range w.events {
		if e.Type == "echo" {
			out = append(out, e)
			if len(out) >= n {
				break
			}
		}
	}
	return out
}
