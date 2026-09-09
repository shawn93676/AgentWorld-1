// Package village —— Agent Village 世界领域层（AV-01 · Willow Creek）。
//
// 设计要点（对应 docs：一个 Village = 一个逻辑 Actor）：
//   - 全部世界状态在内存中，所有变更经由 World.mu 串行化（Village Actor 语义）；
//   - Agent 元数据（名字/性格/目标）存 SQLite 供 Scheduler 调度，富状态（金钱/位置/
//     关系/记忆）留在本包，由 JSON 快照持久化；
//   - 世界时钟：每真实 tick 推进 Speed 个游戏分钟（默认 10，即现实 1 分钟 = 游戏 10 分钟
//     的近 3 倍速演示档，可通过 /api/speed 调节）。
package village

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"time"

	"agentworld/internal/life"
	"agentworld/worlds/goosegame/goose"
)

// VillageName 村庄名。
const VillageName = "Willow Creek"

// Place 一个地点。
type Place struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Emoji string   `json:"emoji"`
	Desc  string   `json:"desc"`
	X     float64  `json:"x"` // SVG 地图坐标（0..360 / 0..520）
	Y     float64  `json:"y"`
	Links []string `json:"-"`
}

// Relationship 一条单向关系（G0 §9：Like/Trust 而非 friend=true）。
type Relationship struct {
	Like  int `json:"like"`  // 0..100
	Trust int `json:"trust"` // 0..100
}

// Memory 一条记忆（会写入快照并影响决策与聊天）。
//
// Src/Tag/EchoDay/Echoed 服务于"长程因果"：玩家干预留下的记忆被标记为
// src=player，并约定一个回响日，到那天世界会把它重新提起（见 echo.go）。
// 老快照没有这些字段 → 零值，按普通记忆处理。
type Memory struct {
	Day     int    `json:"day"`
	Minute  int    `json:"minute"`
	Text    string `json:"text"`
	Imp     int    `json:"imp"`                // 1~5
	Src     string `json:"src,omitempty"`      // "player" = 玩家留下的痕迹
	Tag     string `json:"tag,omitempty"`      // 干预类型，决定回响台词
	EchoDay int    `json:"echo_day,omitempty"` // 计划回响日
	Echoed  bool   `json:"echoed,omitempty"`
}

// Plan 玩家 Influence 被接受后挂起的计划（Agent 会在后续决策中执行它）。
type Plan struct {
	Text   string `json:"text"`
	GoTo   string `json:"go_to,omitempty"`
	TalkTo string `json:"talk_to,omitempty"` // 目标 Agent 名字
	DueDay int    `json:"due_day"`
}

// Agent 一个村民（富状态，内存中）。
type Agent struct {
	ID          int64
	Name        string
	Occupation  string
	Emoji       string
	Personality []string
	Money       int64
	Energy      int // 0..100
	Mood        int // -100..100
	Skills      map[string]int
	Goal        string
	GoalTarget  int64
	GoalDone    bool
	Workplace   string
	Home        string
	BedHour     int
	WakeHour    int
	Social      float64  // 0..1 爱社交
	Grit        float64  // 0..1 倔强（影响 Influence 接受率）
	Lines       []string // 闲聊模板句（人设）
	Place       string
	ActKind     string    // work / sleep / eat / social / travel / rest / shop / idle
	Action      string    // 给人看的当前动作
	TravelTo    string    // travel 中的目的地
	NextAct     *Activity // travel 到达后要进入的活动（不持久化）
	ActionUntil int64     // 绝对游戏分钟
	Due         bool      // 需要重新决策
	Gone        bool      // 已离开村庄（不可逆：债务随之作废）
	Life        life.LifeState // 跨 World 生命周期：alive/sleeping/traveling/dead/archived。由 life 包管理，Think 循环据此守卫。
	Rel         map[int64]*Relationship
	Owes        map[int64]int64 // 我欠别人（agentID → 金币）
	Mem         []Memory        // 新的在前
	Plan        *Plan
	LastChatAt  map[int64]int64 // 与某 Agent 最近一次互动的绝对分钟
	rentPaidDay int
	lastWarnDay int // 上次发出"撑不住了"预兆的日子（每天至多一次）
	// Attach 阶段的暂存：等所有 Agent 到齐后在 SealRelations 里按名字编织。
	snapRel  map[string]relSnap // 快照恢复的关系（优先于种子）
	snapOwes map[string]int64   // 快照恢复的债务
}

// Event 一条世界事件（G0 §10/§11：带"Why"的故事流）。
type Event struct {
	ID     int64    `json:"id"`
	Day    int      `json:"day"`
	Minute int      `json:"minute"`
	Clock  string   `json:"clock"`
	Type   string   `json:"type"`
	Icon   string   `json:"icon"`
	Actor  string   `json:"actor,omitempty"`
	Target string   `json:"target,omitempty"`
	Place  string   `json:"place,omitempty"`
	Text   string   `json:"text"`
	Why    []string `json:"why,omitempty"`
}

// Player 玩家（Observer / Influencer）。V0.1 单玩家免登录。
type Player struct {
	Name      string `json:"name"`
	Gold      int64  `json:"gold"`
	Given     int64  `json:"given"`      // 玩家赠予村民的累计金币
	Borrowed  int64  `json:"borrowed"`   // 玩家向村民借/讨的累计金币（不记反向债务）
	Chats     int64  `json:"chats"`
	ActDay int    `json:"act_day"` // 行动额度所属日（跨日自动重置）
	Acts   int    `json:"acts"`    // 当日已用行动数
}

// ChatLLM 玩家对话的可选高级模型后端（Level 2 决策），由 module 注入适配器。
type ChatLLM interface {
	Chat(ctx context.Context, system, user string) (string, error)
	Enabled() bool
}

// World 村庄世界。
type World struct {
	mu        sync.Mutex
	obs       *goose.Observatory
	rng       *rand.Rand
	now       int64 // 绝对游戏分钟（Day 1 00:00 起算）
	speed     int   // 每 tick 推进的游戏分钟
	weather   string
	places    map[string]*Place
	placeOrd  []string
	agents    map[int64]*Agent
	agentOrd  []int64
	events    []Event // 新的在前
	evSeq     int64
	threads   []Thread // 悬念线程（Cliffhook）；未决的在前
	threadSeq int64
	player    Player
	dailyActs int                  // 玩家每日行动额度（G0 §20 Free 档）
	repair    bool                 // Market 失火待修
	chatLLM   ChatLLM              // 可选 LLM 聊天后端
	savePath  string               // 快照路径
	pending   map[string]agentSnap // Load 后等待 Attach 恢复的数据
	seeds     map[int64]Profile    // Attach 暂存的人设（SealRelations 用）
	tickN     int64
}

// ---- 时钟 ----

func (w *World) Day() int      { return int(w.now/1440) + 1 }
func (w *World) Minute() int   { return int(w.now % 1440) }
func (w *World) AbsMin() int64 { return w.now }
func (w *World) Clock() string {
	return fmt.Sprintf("%02d:%02d", w.Minute()/60, w.Minute()%60)
}

// MoodEmoji 心情表情。
func MoodEmoji(m int) string {
	switch {
	case m >= 60:
		return "😄"
	case m >= 30:
		return "😊"
	case m >= 5:
		return "🙂"
	case m >= -15:
		return "😐"
	case m >= -40:
		return "😟"
	default:
		return "😠"
	}
}

// ---- 构建 ----

// NewWorld 创建世界（先 NewWorld 再逐个 Attach Agent）。
func NewWorld(obs *goose.Observatory, savePath string) *World {
	w := &World{
		obs:       obs,
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
		now:       7*60 + 50, // Day 1 07:50 清晨开村
		speed:     10,
		weather:   "Sunny",
		places:    map[string]*Place{},
		agents:    map[int64]*Agent{},
		player:    Player{Name: "Traveler", Gold: 500},
		dailyActs: defaultDailyActs,
		savePath:  savePath,
		pending:   map[string]agentSnap{},
		seeds:     map[int64]Profile{},
	}
	w.initPlaces()
	w.Load() // 有快照则恢复
	if w.evSeq == 0 {
		w.addEvent(Event{Type: "day", Icon: "🏘️",
			Text: "A Traveler arrives at Willow Creek. The village is waking up.",
			Why:  []string{"World booted", "V0.1 seed"}})
	}
	return w
}

// Attach 把一个 DB Agent 挂进世界（存在同名快照数据则恢复富状态）。
// 关系与债务的编织延迟到 SealRelations（此时全部 Agent 已到齐，可按名字解析）。
func (w *World) Attach(id int64, p Profile) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := &Agent{
		ID: id, Name: p.Name, Occupation: p.Occupation, Emoji: p.Emoji,
		Personality: p.Personality, Money: p.Money, Energy: 70, Mood: 20,
		Skills: p.Skills, Goal: p.Goal, GoalTarget: p.GoalTarget,
		Workplace: p.Workplace, Home: "residential",
		BedHour: p.BedHour, WakeHour: p.WakeHour,
		Social: p.Social, Grit: p.Grit, Lines: p.Lines,
		Place: p.Workplace, ActKind: "idle", Action: "Just woke up",
		Life: life.LifeAlive, // 出生即在世
		Rel: map[int64]*Relationship{}, Owes: map[int64]int64{},
		LastChatAt: map[int64]int64{},
	}
	for _, m := range p.SeedMem {
		a.Mem = append(a.Mem, Memory{Day: 1, Minute: 0, Text: m, Imp: 3})
	}
	// 恢复快照（非关系字段）
	if snap, ok := w.pending[a.Name]; ok {
		a.Money, a.Energy, a.Mood = snap.Money, snap.Energy, snap.Mood
		a.Gone = snap.Gone
		a.Place, a.ActKind, a.Action, a.TravelTo = snap.Place, snap.ActKind, snap.Action, snap.TravelTo
		a.ActionUntil, a.GoalDone = snap.ActionUntil, snap.GoalDone
		a.Plan = snap.Plan
		a.Mem = snap.Mem
		if len(snap.Skills) > 0 {
			sk := make(map[string]int, len(snap.Skills))
			for k, v := range snap.Skills {
				sk[k] = v
			}
			a.Skills = sk
		}
		a.Due = true
		a.snapRel, a.snapOwes = snap.Rel, snap.Owes
	}
	w.agents[id] = a
	w.agentOrd = append(w.agentOrd, id)
	w.seeds[id] = p
}

// SealRelations 在全部 Attach 完成后调用一次：
// 1) 应用种子关系/债务（此时名字 → ID 全部可解析）；
// 2) 快照恢复的关系/债务覆盖种子（世界连续性优先于初始剧本）；
// 3) 补全缺失的反向关系为中性值。
func (w *World) SealRelations() {
	w.mu.Lock()
	defer w.mu.Unlock()
	byName := map[string]int64{}
	for _, id := range w.agentOrd {
		byName[w.agents[id].Name] = id
	}
	apply := func(a *Agent, rel map[string]relSnap, owes map[string]int64) {
		for name, r := range rel {
			if oid, ok := byName[name]; ok {
				a.Rel[oid] = &Relationship{Like: r.Like, Trust: r.Trust}
			}
		}
		for name, amt := range owes {
			if oid, ok := byName[name]; ok {
				a.Owes[oid] = amt
			}
		}
	}
	for _, id := range w.agentOrd {
		a := w.agents[id]
		p := w.seeds[id]
		seedRel := map[string]relSnap{}
		for name, r := range p.SeedRel {
			seedRel[name] = relSnap{Like: r.Like, Trust: r.Trust}
		}
		owes := map[string]int64{}
		for name, amt := range p.Owes {
			owes[name] = amt
		}
		apply(a, seedRel, owes)
		if a.snapRel != nil || a.snapOwes != nil {
			apply(a, a.snapRel, a.snapOwes) // 快照优先
			a.snapRel, a.snapOwes = nil, nil
		}
	}
	for _, id := range w.agentOrd {
		a := w.agents[id]
		for oid := range a.Rel {
			if other := w.agents[oid]; other != nil && other.Rel[a.ID] == nil {
				other.Rel[a.ID] = &Relationship{Like: 40, Trust: 40}
			}
		}
	}
}

// SetChatLLM 注入玩家对话 LLM（nil = 纯模板回复）。
func (w *World) SetChatLLM(c ChatLLM) { w.mu.Lock(); w.chatLLM = c; w.mu.Unlock() }

// Obs 暴露观察台（SSE 订阅用）。
func (w *World) Obs() *goose.Observatory { return w.obs }

// SetSpeed 设置每 tick 推进的游戏分钟（1..60）。
func (w *World) SetSpeed(n int) {
	w.mu.Lock()
	if n < 1 {
		n = 1
	}
	if n > 60 {
		n = 60
	}
	w.speed = n
	w.mu.Unlock()
}

// Speed 当前速度。
func (w *World) Speed() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.speed
}

// ---- 事件 ----

func (w *World) addEvent(e Event) {
	w.evSeq++
	e.ID = w.evSeq
	e.Day = w.Day()
	e.Minute = w.Minute()
	e.Clock = w.Clock()
	w.events = append([]Event{e}, w.events...)
	if len(w.events) > 3000 {
		w.events = w.events[:3000]
	}
	if w.obs != nil {
		w.obs.Publish("village.event", e)
	}
}

// FindAgentByName 按名字查找 Agent 的本地 ID（用于定位跨世界旅行的主角，如铁匠 Marcus）。
func (w *World) FindAgentByName(name string) (int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, a := range w.agents {
		if a.Name == name {
			return id, true
		}
	}
	return 0, false
}

// EmitEvent 向世界事件流追加一条事件（用于跨世界旅程的 departure / work / return 可见性）。
func (w *World) EmitEvent(typ, icon, actor, text string, why ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.addEvent(Event{
		Type:  typ,
		Icon:  icon,
		Actor: actor,
		Text:  text,
		Why:   why,
	})
}

// AllEvents 返回全部事件（新的在前，limit 上限 500）。
func (w *World) AllEvents(limit int) []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	if limit <= 0 || limit > 500 {
		limit = 300
	}
	if len(w.events) < limit {
		limit = len(w.events)
	}
	out := make([]Event, limit)
	copy(out, w.events[:limit])
	return out
}

// EventsByDay 返回某天的事件（day<=0 表示今天），新的在前。
func (w *World) EventsByDay(day, limit int) []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	if day <= 0 {
		day = w.Day()
	}
	if limit <= 0 || limit > 500 {
		limit = 300
	}
	out := []Event{}
	for _, e := range w.events {
		if e.Day == day {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// ---- 快照持久化 ----

type relSnap struct {
	Like  int `json:"like"`
	Trust int `json:"trust"`
}

type agentSnap struct {
	Name        string             `json:"name"`
	Money       int64              `json:"money"`
	Energy      int                `json:"energy"`
	Mood        int                `json:"mood"`
	Gone        bool               `json:"gone,omitempty"`
	Place       string             `json:"place"`
	ActKind     string             `json:"act_kind"`
	Action      string             `json:"action"`
	TravelTo    string             `json:"travel_to,omitempty"`
	ActionUntil int64              `json:"action_until"`
	GoalDone    bool               `json:"goal_done"`
	Rel         map[string]relSnap `json:"rel,omitempty"`
	Owes        map[string]int64   `json:"owes,omitempty"`
	Mem         []Memory           `json:"mem"`
	Skills      map[string]int      `json:"skills,omitempty"` // 技能持久化（否则跨重启/快照后技能丢失）
	Plan        *Plan              `json:"plan,omitempty"`
}

type snapshot struct {
	Version   int         `json:"version"`
	Now       int64       `json:"now"`
	Speed     int         `json:"speed"`
	Weather   string      `json:"weather"`
	Player    Player      `json:"player"`
	Repair    bool        `json:"repair"`
	Agents    []agentSnap `json:"agents"`
	Events    []Event     `json:"events"` // 旧的在前
	Threads   []Thread    `json:"threads,omitempty"`
	ThreadSeq int64       `json:"thread_seq,omitempty"`
}

// Save 落盘快照。
func (w *World) Save() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.saveLocked()
}

func (w *World) saveLocked() error {
	if w.savePath == "" {
		return nil
	}
	snap := snapshot{Version: 1, Now: w.now, Speed: w.speed, Weather: w.weather,
		Player: w.player, Repair: w.repair, Threads: w.threads, ThreadSeq: w.threadSeq}
	nameOf := func(id int64) string {
		if a := w.agents[id]; a != nil {
			return a.Name
		}
		return ""
	}
	for _, id := range w.agentOrd {
		a := w.agents[id]
		s := agentSnap{Name: a.Name, Money: a.Money, Energy: a.Energy, Mood: a.Mood, Gone: a.Gone,
			Place: a.Place, ActKind: a.ActKind, Action: a.Action, TravelTo: a.TravelTo,
			ActionUntil: a.ActionUntil, GoalDone: a.GoalDone, Plan: a.Plan, Mem: a.Mem,
			Rel: map[string]relSnap{}, Owes: map[string]int64{}}
		if len(a.Skills) > 0 {
			sk := make(map[string]int, len(a.Skills))
			for k, v := range a.Skills {
				sk[k] = v
			}
			s.Skills = sk
		}
		for oid, r := range a.Rel {
			if n := nameOf(oid); n != "" {
				s.Rel[n] = relSnap{Like: r.Like, Trust: r.Trust}
			}
		}
		for oid, amt := range a.Owes {
			if n := nameOf(oid); n != "" {
				s.Owes[n] = amt
			}
		}
		snap.Agents = append(snap.Agents, s)
	}
	for i := len(w.events) - 1; i >= 0; i-- { // 旧的在前
		snap.Events = append(snap.Events, w.events[i])
	}
	if len(snap.Events) > 1500 {
		snap.Events = snap.Events[len(snap.Events)-1500:]
	}
	data, err := json.MarshalIndent(snap, "", " ")
	if err != nil {
		return err
	}
	tmp := w.savePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.savePath)
}

// Load 从磁盘恢复快照（无文件或损坏则忽略）。须在 Attach 之前调用。
func (w *World) Load() {
	if w.savePath == "" {
		return
	}
	data, err := os.ReadFile(w.savePath)
	if err != nil {
		return
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil || snap.Version != 1 {
		return
	}
	w.now = snap.Now
	if w.speed <= 0 {
		w.speed = 10
	} else {
		w.speed = snap.Speed
	}
	w.weather = snap.Weather
	w.player = snap.Player
	w.repair = snap.Repair
	w.evSeq = int64(len(snap.Events))
	for i := len(snap.Events) - 1; i >= 0; i-- { // 旧的在前 → 新的在前
		w.events = append(w.events, snap.Events[i])
	}
	// 悬念线程：Version 仍为 1，老快照没有该字段 → 为空，世界照常运行。
	w.threads = snap.Threads
	w.threadSeq = snap.ThreadSeq
	for _, t := range w.threads {
		if t.ID > w.threadSeq {
			w.threadSeq = t.ID
		}
	}
	for _, s := range snap.Agents {
		w.pending[s.Name] = s
	}
}
