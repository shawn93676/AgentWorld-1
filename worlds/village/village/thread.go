package village

import "fmt"

// thread.go —— 悬念线程（Cliffhook）。
//
// 为什么需要这一层：村庄原本只有一条"目标倒计时"主线，10 个 Agent 全部 GoalDone
// 之后故事就讲完了，玩家没有理由再回来。悬念线程让世界持续抛出"未决之事"——
// 玩家离开时总有一个没收尾的事，回来是为了兑现它（G0 §"最重要的一条"的留存闭环）。
//
// 规则：
//   - 世界同时最多 maxOpenThreads 条未决线程；
//   - 每条带 DueDay，到期仍未决 → 按"不插手"自动结算（世界自己走下去，不会卡死）；
//   - 玩家的选择真实改变金钱 / 关系 / 记忆，并产生一条带 Why 的事件。

// 悬念类型。
const (
	threadDebt = "debt" // 债务到期
	threadFund = "fund" // 梦想缺口
	threadFeud = "feud" // 关系失和
	threadOmen = "omen" // 夜话钩子（低成本气氛悬念）
)

// maxOpenThreads 同时未决的悬念上限（太多会变成任务清单，太少会没有回来看的理由）。
const maxOpenThreads = 2

// Thread 一条悬念线程。
type Thread struct {
	ID          int64    `json:"id"`
	Kind        string   `json:"kind"`
	Icon        string   `json:"icon"`
	Actor       string   `json:"actor,omitempty"`
	Target      string   `json:"target,omitempty"`
	Title       string   `json:"title"`   // 一句话悬念
	Detail      string   `json:"detail"`  // 现状说明（让玩家判断该不该插手）
	Options     []string `json:"options"` // 玩家可选项，最后一项恒为"不插手"
	CreatedDay  int      `json:"created_day"`
	DueDay      int      `json:"due_day"` // 到期日（含）
	Resolved    bool     `json:"resolved"`
	Choice      string   `json:"choice,omitempty"`
	Outcome     string   `json:"outcome,omitempty"`
	ResolvedDay int      `json:"resolved_day,omitempty"`
}

// Threads 全部悬念线程（未决在前，已了结在后）。
func (w *World) Threads() []Thread {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.threadViewsLocked()
}

// DecideThread 玩家对一条悬念做出选择。choice 必须是该线程的选项之一。
func (w *World) DecideThread(id int64, choice string) map[string]interface{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.threads {
		t := &w.threads[i]
		if t.ID != id || t.Resolved {
			continue
		}
		ok := false
		for _, o := range t.Options {
			if o == choice {
				ok = true
				break
			}
		}
		if !ok {
			return nil
		}
		q, allowed := w.spendActLocked()
		if !allowed {
			return actDenied(q)
		}
		w.settleThreadLocked(t, choice)
		return map[string]interface{}{
			"thread": *t, "player_gold": w.player.Gold, "quota": q,
		}
	}
	return nil
}

// ---- 推进 ----

// turnThreadsLocked 推进悬念：先结算到期的，再补充新的。须持锁调用。
func (w *World) turnThreadsLocked() {
	today := w.Day()
	for i := range w.threads {
		t := &w.threads[i]
		if t.Resolved {
			continue
		}
		if g := w.goneInThreadLocked(t); g != nil {
			w.settleThreadLocked(t, "departed") // 当事人走了，事情随他而去
			continue
		}
		if today > t.DueDay {
			w.settleThreadLocked(t, "") // 超时 = 世界自己走下去
		}
	}
	w.spawnThreadsLocked()
	w.pruneThreadsLocked()
}

func (w *World) spawnThreadsLocked() {
	gens := []func() (Thread, bool){w.genDebt, w.genFeud, w.genFund, w.genOmen}
	for _, g := range gens {
		if w.openThreadCountLocked() >= maxOpenThreads {
			return
		}
		t, ok := g()
		if !ok {
			continue
		}
		w.threadSeq++
		t.ID = w.threadSeq
		t.CreatedDay = w.Day()
		w.threads = append(w.threads, t)
		w.addEvent(Event{Type: "thread", Icon: t.Icon, Actor: t.Actor, Target: t.Target,
			Text: t.Title,
			Why: []string{"A matter left unresolved",
				fmt.Sprintf("Settles by day %d if nobody steps in", t.DueDay)}})
	}
}

func (w *World) pruneThreadsLocked() {
	if len(w.threads) <= 24 {
		return
	}
	open := []Thread{}
	done := []Thread{}
	for _, t := range w.threads {
		if t.Resolved {
			done = append(done, t)
		} else {
			open = append(open, t)
		}
	}
	if len(done) > 12 {
		done = done[len(done)-12:] // 保留最近的了结
	}
	w.threads = append(open, done...)
}

func (w *World) threadViewsLocked() []Thread {
	open := []Thread{}
	done := []Thread{}
	for _, t := range w.threads {
		if t.Resolved {
			done = append(done, t)
		} else {
			open = append(open, t)
		}
	}
	return append(open, done...)
}

func (w *World) openThreadCountLocked() int {
	n := 0
	for _, t := range w.threads {
		if !t.Resolved {
			n++
		}
	}
	return n
}

func (w *World) hasOpenKindLocked(kind string) bool {
	for _, t := range w.threads {
		if !t.Resolved && t.Kind == kind {
			return true
		}
	}
	return false
}

func (w *World) hasOpenActorLocked(name string) bool {
	for _, t := range w.threads {
		if !t.Resolved && (t.Actor == name || t.Target == name) {
			return true
		}
	}
	return false
}

// ---- 生成器（全部基于真实世界状态，保证"这件事真的悬着"）----

// genDebt 债务悬念：有人欠钱且还没还。
func (w *World) genDebt() (Thread, bool) {
	if w.hasOpenKindLocked(threadDebt) {
		return Thread{}, false
	}
	for _, id := range w.agentOrd {
		a := w.agents[id]
		if a.Gone || w.hasOpenActorLocked(a.Name) {
			continue
		}
		for oid, amt := range a.Owes {
			if amt <= 0 {
				continue
			}
			c := w.agents[oid]
			if c == nil || c.Gone {
				continue
			}
			detail := fmt.Sprintf("%s is holding %d gold. The debt is %d.", a.Name, a.Money, amt)
			if short := amt - a.Money; short > 0 {
				detail += fmt.Sprintf(" He is %d short.", short)
			} else {
				detail += " He could pay today — if he wants to."
			}
			return Thread{Kind: threadDebt, Icon: "⚡", Actor: a.Name, Target: c.Name,
				Title:   fmt.Sprintf("%s swore to %s: \"%d gold, soon.\" The village is watching.", a.Name, c.Name, amt),
				Detail:  detail,
				Options: []string{"Pay it for him", "Press him", "Stay out of it"},
				DueDay:  w.Day() + 2}, true
		}
	}
	return Thread{}, false
}

// genFeud 失和悬念：两个村民已经互相看不顺眼。
func (w *World) genFeud() (Thread, bool) {
	if w.hasOpenKindLocked(threadFeud) {
		return Thread{}, false
	}
	for i := 0; i < len(w.agentOrd); i++ {
		for j := i + 1; j < len(w.agentOrd); j++ {
			a, b := w.agents[w.agentOrd[i]], w.agents[w.agentOrd[j]]
			if a == nil || b == nil || a.Gone || b.Gone ||
				w.hasOpenActorLocked(a.Name) || w.hasOpenActorLocked(b.Name) {
				continue
			}
			ra, rb := a.Rel[b.ID], b.Rel[a.ID]
			if ra == nil || rb == nil {
				continue
			}
			if ra.Like > 26 || rb.Like > 26 {
				continue
			}
			return Thread{Kind: threadFeud, Icon: "⚡", Actor: a.Name, Target: b.Name,
				Title:   fmt.Sprintf("%s and %s can no longer share a room without sparks.", a.Name, b.Name),
				Detail:  fmt.Sprintf("They rate each other %d and %d out of 100. The whole village feels it.", ra.Like, rb.Like),
				Options: []string{"Mediate", "Side with " + a.Name, "Stay out of it"},
				DueDay:  w.Day() + 2}, true
		}
	}
	return Thread{}, false
}

// genFund 梦想缺口：某人离目标只差一口气。
func (w *World) genFund() (Thread, bool) {
	if w.hasOpenKindLocked(threadFund) {
		return Thread{}, false
	}
	for _, id := range w.agentOrd {
		a := w.agents[id]
		if a == nil || a.Gone || a.GoalTarget <= 0 || a.GoalDone || a.GoalAbandoned || w.hasOpenActorLocked(a.Name) {
			continue
		}
		gap := a.GoalTarget - a.Money
		if gap <= 0 || gap > 150 {
			continue
		}
		return Thread{Kind: threadFund, Icon: "⚡", Actor: a.Name,
			Title:   fmt.Sprintf("%s needs %d more gold to %s.", a.Name, gap, lowerFirst(a.Goal)),
			Detail:  fmt.Sprintf("He has %d of the %d gold he needs.", a.Money, a.GoalTarget),
			Options: []string{"Fund him", "Find him work", "Stay out of it"},
			DueDay:  w.Day() + 2}, true
	}
	return Thread{}, false
}

// genOmen 夜话钩子：不讲道理的悬念，专门制造"明天会发生什么"。
func (w *World) genOmen() (Thread, bool) {
	if w.hasOpenKindLocked(threadOmen) {
		return Thread{}, false
	}
	return Thread{Kind: threadOmen, Icon: "🌙",
		Title: pickStr(w.rng, []string{
			"Someone was seen slipping into the Church after midnight. Grace won't say who.",
			"The Mine has gone quiet for two days. William answers questions with silence.",
			"A sealed letter was left on the Tavern bar. It is addressed to nobody.",
			"Marco has started counting the carts twice. He won't explain why.",
		}),
		Detail:  "Nothing has happened yet. That is exactly what makes it strange.",
		Options: []string{"Ask around", "Ignore it"},
		DueDay:  w.Day() + 2}, true
}

// ---- 结算 ----

// settleThreadLocked 结算一条悬念。choice 为空表示超时，按"不插手"处理。
// goneInThreadLocked 返回这条悬念里已经离开村庄的当事人（没有则 nil）。
func (w *World) goneInThreadLocked(t *Thread) *Agent {
	for _, n := range []string{t.Actor, t.Target} {
		if n == "" {
			continue
		}
		if a := w.agentByName(n); a != nil && a.Gone {
			return a
		}
	}
	return nil
}

func (w *World) settleThreadLocked(t *Thread, choice string) {
	if choice == "departed" {
		if g := w.goneInThreadLocked(t); g != nil {
			t.Resolved = true
			t.Choice = "departed"
			t.ResolvedDay = w.Day()
			t.Outcome = fmt.Sprintf("%s left Willow Creek before this could be settled. The matter ended with his leaving.", g.Name)
			w.addEvent(Event{Type: "thread_done", Icon: "🚪", Actor: t.Actor, Target: t.Target,
				Text: t.Outcome,
				Why:  []string{"The person at the centre of it is gone", "Nothing left to decide"}})
			return
		}
	}
	if choice == "" && len(t.Options) > 0 {
		choice = t.Options[len(t.Options)-1]
	}
	t.Resolved = true
	t.Choice = choice
	t.ResolvedDay = w.Day()
	switch t.Kind {
	case threadDebt:
		w.settleDebtLocked(t, choice)
	case threadFund:
		w.settleFundLocked(t, choice)
	case threadFeud:
		w.settleFeudLocked(t, choice)
	case threadOmen:
		w.settleOmenLocked(t, choice)
	}
	if t.Outcome == "" {
		t.Outcome = "The matter quietly faded."
	}
	tr := &WhyTrace{
		Rule:     "thread.choice",
		Cause:    []string{fmt.Sprintf("kind=%s", t.Kind), fmt.Sprintf("choice=%s", choice)},
		Recent:   []string{t.Title},
		Decision: fmt.Sprintf("resolved thread with choice: %s", choice),
	}
	w.addEvent(Event{Type: "thread_done", Icon: "✅", Actor: t.Actor, Target: t.Target,
		Text: t.Outcome,
		Why:  []string{"A promise came due", "Choice: " + choice},
		Trace: tr})
	if t.Actor != "" {
		w.logWhy(t.Actor, "thread.choice", tr, trunc(t.Outcome, 80))
	}
}

func (w *World) settleDebtLocked(t *Thread, choice string) {
	d, c := w.agentByName(t.Actor), w.agentByName(t.Target)
	if d == nil || c == nil {
		t.Outcome = "The debt was forgotten by everyone involved."
		return
	}
	if d.Gone || c.Gone {
		t.Outcome = "One of them is no longer in Willow Creek. The debt went with him."
		return
	}
	amt := d.Owes[c.ID]
	if amt <= 0 {
		t.Outcome = fmt.Sprintf("%s had already settled with %s. Nothing left to decide.", d.Name, c.Name)
		return
	}
	pr := playerRel(d)

	if choice == "Pay it for him" {
		if w.player.Gold < amt {
			t.Outcome = fmt.Sprintf("You did not have %d gold, so you could only press %s to find it himself.", amt, d.Name)
			choice = "催他还钱"
		} else {
			w.player.Gold -= amt
			w.player.Given += amt
			c.Money += amt
			delete(d.Owes, c.ID)
			w.adjustRel(c, d, 10, 12)
			w.adjustRel(d, c, 6, 8)
			pr.Like = clamp(pr.Like+12, 0, 100)
			pr.Trust = clamp(pr.Trust+10, 0, 100)
			d.Mood = clamp(d.Mood+12, -100, 100)
			w.rememberPlayer(d, fmt.Sprintf("The Traveler paid %s the %d gold I owed. I owe the Traveler more than coin now.", c.Name, amt), 5, "debt_pay")
			w.remember(c, fmt.Sprintf("%s's debt is settled — the Traveler paid it out of his own purse.", d.Name), 4)
			t.Outcome = fmt.Sprintf("You paid %s's %d gold debt to %s yourself. He will not forget it.", d.Name, amt, c.Name)
			return
		}
	}

	if choice == "Press him" {
		if d.Money >= amt {
			d.Money -= amt
			c.Money += amt
			delete(d.Owes, c.ID)
			w.adjustRel(c, d, 6, 8)
			w.adjustRel(d, c, -4, 2)
			d.Mood = clamp(d.Mood-6, -100, 100)
			w.rememberPlayer(d, "The Traveler pressed me about the debt. I paid it, but it stung.", 4, "debt_press")
			w.remember(c, fmt.Sprintf("%s finally repaid me — but only after the Traveler leaned on him.", d.Name), 4)
			t.Outcome = fmt.Sprintf("Under your pressure, %s repaid the %d gold. The coin moved; the warmth did not.", d.Name, amt)
		} else {
			w.adjustRel(c, d, -4, -3)
			w.adjustRel(d, c, -6, -5)
			d.Mood = clamp(d.Mood-14, -100, 100)
			c.Mood = clamp(c.Mood-6, -100, 100)
			w.remember(d, fmt.Sprintf("The Traveler pressed me for the %d gold, but my purse was empty. Shame on me.", amt), 4)
			w.remember(c, fmt.Sprintf("%s still cannot pay. My patience is running out.", d.Name), 4)
			t.Outcome = fmt.Sprintf("%s simply does not have the %d gold. Pressing him only made it worse for everyone.", d.Name, amt)
		}
		return
	}

	// 不插手：世界自己走
	if d.Money >= amt && w.rng.Float64() < 0.5 {
		d.Money -= amt
		c.Money += amt
		delete(d.Owes, c.ID)
		w.adjustRel(c, d, 8, 10)
		w.remember(c, fmt.Sprintf("%s repaid me in the end. Maybe I judged him too hard.", d.Name), 4)
		t.Outcome = fmt.Sprintf("You stayed out of it. %s repaid the %d gold before the day was out.", d.Name, amt)
	} else {
		w.adjustRel(c, d, -5, -4)
		w.adjustRel(d, c, -3, -4)
		c.Mood = clamp(c.Mood-8, -100, 100)
		w.remember(c, fmt.Sprintf("%s let the day pass without paying. Again.", d.Name), 4)
		w.remember(d, fmt.Sprintf("I could not face %s today. The %d gold is still sitting there.", c.Name, amt), 3)
		t.Outcome = fmt.Sprintf("You stayed out of it. %s broke his word again, and %s's patience is thinning.", d.Name, c.Name)
	}
}

func (w *World) settleFundLocked(t *Thread, choice string) {
	a := w.agentByName(t.Actor)
	if a == nil || a.Gone || a.GoalDone || a.GoalAbandoned {
		t.Outcome = "The matter quietly faded."
		return
	}
	gap := a.GoalTarget - a.Money
	if gap <= 0 {
		t.Outcome = fmt.Sprintf("%s got there on his own after all.", a.Name)
		return
	}
	pr := playerRel(a)

	if choice == "Fund him" {
		give := gap
		if give > w.player.Gold {
			give = w.player.Gold
		}
		if give > 0 {
			w.player.Gold -= give
			w.player.Given += give
			a.Money += give
			pr.Like = clamp(pr.Like+6, 0, 100)
			pr.Trust = clamp(pr.Trust+4, 0, 100)
			a.Mood = clamp(a.Mood+10, -100, 100)
			w.rememberPlayer(a, fmt.Sprintf("The Traveler gave me %d gold when I was short. I will repay it in honest work.", give), 5, "fund_give")
			t.Outcome = fmt.Sprintf("You gave %s the %d gold he needed. He is now %d/%d toward \"%s\".",
				a.Name, give, a.Money, a.GoalTarget, a.Goal)
			return
		}
		choice = "Find him work" // 玩家自己也掏不出来
	}

	if choice == "Find him work" {
		extra := w.wageFor(a) * 2
		a.Money += extra
		a.Energy = clamp(a.Energy-12, 0, 100)
		a.Mood = clamp(a.Mood-4, -100, 100)
		pr.Like = clamp(pr.Like+2, 0, 100)
		w.rememberPlayer(a, fmt.Sprintf("The Traveler found me extra work. I earned %d gold and lost a night's sleep.", extra), 3, "fund_work")
		t.Outcome = fmt.Sprintf("%s took two extra shifts and earned %d gold — and lost a night's sleep doing it.", a.Name, extra)
		return
	}

	// 不插手
	if w.rng.Float64() < 0.35 {
		scraped := int64(float64(gap) * 0.5)
		a.Money += scraped
		a.Mood = clamp(a.Mood+4, -100, 100)
		w.remember(a, "I scraped together a little more for the dream. Slowly, slowly.", 3)
		t.Outcome = fmt.Sprintf("You stayed out of it. %s scraped together %d gold on his own.", a.Name, scraped)
	} else {
		a.Mood = clamp(a.Mood-5, -100, 100)
		w.remember(a, "Another day, another shortfall. Nobody is coming to help.", 2)
		t.Outcome = fmt.Sprintf("You stayed out of it. %s is still %d gold short, and the waiting is wearing on him.", a.Name, gap)
	}
}

func (w *World) settleFeudLocked(t *Thread, choice string) {
	a, b := w.agentByName(t.Actor), w.agentByName(t.Target)
	if a == nil || b == nil || a.Gone || b.Gone {
		t.Outcome = "The quarrel ended because one of them is no longer here."
		return
	}
	switch {
	case choice == "Mediate":
		w.adjustRel(a, b, 10, 6)
		w.adjustRel(b, a, 10, 6)
		a.Mood = clamp(a.Mood+6, -100, 100)
		b.Mood = clamp(b.Mood+6, -100, 100)
		playerRel(a).Trust = clamp(playerRel(a).Trust+5, 0, 100)
		playerRel(b).Trust = clamp(playerRel(b).Trust+5, 0, 100)
		w.rememberPlayer(a, "The Traveler sat us down together. It helped more than I expected.", 4, "mediate")
		w.rememberPlayer(b, "The Traveler made us talk. Maybe it was worth it.", 4, "mediate")
		t.Outcome = fmt.Sprintf("You sat %s and %s down together. Not friendship yet — but the shouting has stopped.", a.Name, b.Name)
	case choice == "Side with "+a.Name:
		w.adjustRel(a, b, -6, -6)
		w.adjustRel(b, a, -10, -8)
		playerRel(a).Like = clamp(playerRel(a).Like+6, 0, 100)
		playerRel(b).Like = clamp(playerRel(b).Like-8, 0, 100)
		b.Mood = clamp(b.Mood-10, -100, 100)
		w.rememberPlayer(b, "The Traveler took his side against me. I won't forget that.", 4, "side")
		w.rememberPlayer(a, "The Traveler stood by me. That means something.", 3, "sided_with")
		t.Outcome = fmt.Sprintf("You sided with %s. He is grateful — and %s has not forgotten it.", a.Name, b.Name)
	default: // 不插手
		w.adjustRel(a, b, -6, -4)
		w.adjustRel(b, a, -6, -4)
		a.Mood = clamp(a.Mood-6, -100, 100)
		b.Mood = clamp(b.Mood-6, -100, 100)
		w.remember(a, fmt.Sprintf("Things with %s keep souring. Nobody helps.", b.Name), 3)
		w.remember(b, fmt.Sprintf("Things with %s keep souring. Nobody helps.", a.Name), 3)
		t.Outcome = fmt.Sprintf("Nobody stepped in. %s and %s have drifted further apart.", a.Name, b.Name)
	}
}

func (w *World) settleOmenLocked(t *Thread, choice string) {
	if choice != "Ask around" {
		t.Outcome = "You let it go. Whatever it was, the village moved on without you."
		return
	}
	cands := []*Agent{}
	for _, id := range w.agentOrd {
		if a := w.agents[id]; a != nil && !a.Gone && a.ActKind != "sleep" {
			cands = append(cands, a)
		}
	}
	if len(cands) == 0 {
		t.Outcome = "You asked around, but the whole village kept its mouth shut."
		return
	}
	a := cands[w.rng.Intn(len(cands))]
	pr := playerRel(a)
	pr.Like = clamp(pr.Like+4, 0, 100)
	pr.Trust = clamp(pr.Trust+3, 0, 100)
	if w.rng.Float64() < 0.5 {
		found := int64(5 + w.rng.Intn(16))
		w.player.Gold += found
		w.rememberPlayer(a, "I told the Traveler what I had seen. He slipped me a few coins for the trouble.", 3, "omen_ask")
		t.Outcome = fmt.Sprintf("%s talked. The story was small — but it came with %d gold someone had dropped.", a.Name, found)
		return
	}
	w.rememberPlayer(a, "The Traveler asked about the noises. I told him what I know.", 2, "omen_ask")
	t.Outcome = fmt.Sprintf("%s talked. Nothing came of it, but he trusts you a little more now.", a.Name)
}
