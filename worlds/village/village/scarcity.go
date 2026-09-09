package village

import "fmt"

// scarcity.go —— 稀缺与不可逆：让玩家的每一个选择都有代价。
//
// 两条腿：
//
//  1. 每日行动额度（G0 §20 已经写好的 Free 档设计）。玩家的交互不是无限的，
//     用完就只能等明天——这本身就是"明天要回来"的时钟，也让"今天先做哪一件"
//     变成一个真正的取舍。
//
//  2. 不可逆后果。村民会因为债务与绝望真的离开村庄，而且一旦离开：
//     他欠的钱永远收不回来（债主真的损失），别人欠他的也随之作废。
//     离开前有预兆事件，玩家来得及救——但也可能来不及。
//
// 合起来的效果：催债催太狠，人可能会跑。这就是"选择有重量"。

// defaultDailyActs 默认每日行动额度（对话 / 建议 / 赠礼 / 悬念抉择各计 1 次）。
const defaultDailyActs = 20

// 离开村庄的阈值。到达 warn 线会先出现预兆，给玩家留出救援窗口。
const (
	leaveDebt     = -30 // 负债到这个数
	leaveDespair  = -85 // 或绝望到这个数
	warnDebt      = -15 // 预兆线
	warnDespair   = -70
	minVillagers  = 3 // 村庄至少保留这么多村民，世界不会空掉
	fateGraceDays = 3 // 开局缓冲天数
)

// Quota 玩家每日行动额度。
type Quota struct {
	Limit int `json:"limit"`
	Used  int `json:"used"`
	Left  int `json:"left"`
	Day   int `json:"day"`
}

// SetDailyActs 调整每日行动额度。
func (w *World) SetDailyActs(n int) {
	w.mu.Lock()
	w.dailyActs = n
	w.mu.Unlock()
}

// Quota 当前额度（跨日自动重置）。
func (w *World) Quota() Quota {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.quotaLocked()
}

func (w *World) quotaLocked() Quota {
	if w.player.ActDay != w.Day() {
		w.player.ActDay = w.Day()
		w.player.Acts = 0
	}
	left := w.dailyActs - w.player.Acts
	if left < 0 {
		left = 0
	}
	return Quota{Limit: w.dailyActs, Used: w.player.Acts, Left: left, Day: w.Day()}
}

// spendActLocked 消耗一次行动额度；额度用尽返回 false。须持锁调用。
func (w *World) spendActLocked() (Quota, bool) {
	q := w.quotaLocked()
	if q.Left <= 0 {
		return q, false
	}
	w.player.Acts++
	return w.quotaLocked(), true
}

// actDenied 额度耗尽时的统一返回。
func actDenied(q Quota) map[string]interface{} {
	return map[string]interface{}{
		"error":  "daily_limit",
		"quota":  q,
		"notice": fmt.Sprintf("You have used all %d of today's interactions. Dawn brings new ones.", q.Limit),
	}
}

// ---- 不可逆：债务与绝望会把人逼走 ----

// checkFatesLocked 每日结算：先发预兆，再判定离开。须持锁调用。
func (w *World) checkFatesLocked() {
	if w.Day() <= fateGraceDays {
		return // 给世界一点开局的缓冲
	}
	for _, id := range w.agentOrd {
		a := w.agents[id]
		if a == nil || a.Gone {
			continue
		}
		if a.Money <= leaveDebt || a.Mood <= leaveDespair {
			if w.aliveCountLocked() <= minVillagers {
				return // 保底：不让村庄空掉
			}
			w.departLocked(a)
			continue
		}
		// 还没到不可逆的那一步，但可能已在边缘：给一个能被救回来的信号
		if a.Money <= warnDebt || a.Mood <= warnDespair {
			w.warnOfLeaving(a)
		}
	}
}

func (w *World) aliveCountLocked() int {
	n := 0
	for _, id := range w.agentOrd {
		if a := w.agents[id]; a != nil && !a.Gone {
			n++
		}
	}
	return n
}

// warnOfLeaving 预兆：他快撑不住了。同一天至多一次。
func (w *World) warnOfLeaving(a *Agent) {
	if a.lastWarnDay == w.Day() {
		return
	}
	a.lastWarnDay = w.Day()
	line := fmt.Sprintf("%s sits at home counting coins that are not there anymore. He has stopped coming to the square.", a.Name)
	if a.Money > warnDebt {
		line = fmt.Sprintf("%s barely speaks these days. There is a going-away look in his eyes.", a.Name)
	}
	w.remember(a, "I am running out of reasons to stay. Nobody seems to notice.", 4)
	w.addEvent(Event{Type: "despair", Icon: "🕯️", Actor: a.Name, Place: a.Place, Text: line,
		Why: []string{"Running out of money and spirit",
			fmt.Sprintf("Purse: %d gold · Mood: %s", a.Money, MoodEmoji(a.Mood))}})
}

// departLocked 村民离开村庄（不可逆）。债务随之作废：他欠的钱永远收不回来。
func (w *World) departLocked(a *Agent) {
	a.Gone = true
	a.ActKind = "gone"
	a.Action = "Has left Willow Creek"
	a.Plan = nil

	// 1) 他欠别人的：债主的钱真的没了
	lost := []int64{}
	for oid := range a.Owes {
		lost = append(lost, oid)
	}
	totalLost := int64(0)
	for _, cid := range lost {
		amt := a.Owes[cid]
		delete(a.Owes, cid)
		if amt <= 0 {
			continue
		}
		totalLost += amt
		if c := w.agents[cid]; c != nil && !c.Gone {
			c.Mood = clamp(c.Mood-12, -100, 100)
			w.remember(c, fmt.Sprintf("%s left the village owing me %d gold. That coin is gone for good.", a.Name, amt), 5)
		}
	}
	// 2) 别人欠他的：人走了，账也就散了
	for _, oid := range w.agentOrd {
		o := w.agents[oid]
		if o == nil || o.ID == a.ID {
			continue
		}
		if amt := o.Owes[a.ID]; amt > 0 {
			delete(o.Owes, a.ID)
			o.Mood = clamp(o.Mood+3, -100, 100)
			w.remember(o, fmt.Sprintf("%s left before I could repay him. I am free of it — and ashamed.", a.Name), 3)
		}
	}

	// 3) 悬着的事也随他而去（thread.go 会据此结算）
	suffix := ""
	if totalLost > 0 {
		suffix = fmt.Sprintf(" and %d gold of debts died with him", totalLost)
	}
	w.addEvent(Event{Type: "gone", Icon: "🚪", Actor: a.Name, Place: a.Place,
		Text: fmt.Sprintf("%s has left Willow Creek in the night. The cottage is cold%s.", a.Name, suffix),
		Why: []string{"Debt and despair",
			fmt.Sprintf("Left with %d gold · Mood %s", a.Money, MoodEmoji(a.Mood))}})
}
