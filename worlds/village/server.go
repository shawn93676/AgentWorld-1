// server.go —— Agent Village 的 HTTP + SSE 服务（复用 economy/goosegame 模式）。
//
// REST：
//
//	GET  /api/state            → 全量快照（村庄时钟 / 地点+村民 / 玩家）
//	GET  /api/agents/{id}      → 村民深度视图（关系 / 记忆 / 相关事件）
//	POST /api/agents/{id}/chat     {message}      → 对话（进记忆 / 关系 / 事件）
//	POST /api/agents/{id}/influence {advice}      → 建议（Agent 可拒绝）
//	POST /api/agents/{id}/gift     {gold}         → 赠礼
//	POST /api/agents/{id}/borrow   {gold}         → 向村民讨/借金币（真实移动金币，不记债务）
//	GET  /api/events?day=N      → 某天事件（Daily Story）
//	GET  /api/threads           → 悬念线程（未决的在前）
//	POST /api/threads/{id}/decide {choice}        → 对一条悬念做出选择
//	POST /api/speed             {speed}           → 世界流速（每 tick 游戏分钟）
//	GET  /api/whylog?days=N     → Causal Life Log（重要决定的结构化因果：为什么离开/完成目标/接受影响/做选择）
//	GET  /api/stream            → SSE（village.state / village.event）
//
// 每个游客（cookie wc_uid）拥有独立世界，所有 handler 先解析其世界实例再处理。
// 前端：webstatic/dist 的内嵌产物；未构建时不影响 API。
package village

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"agentworld/internal/life"
	"agentworld/worlds/village/village"
	"agentworld/worlds/village/webstatic"
)

// Server 村庄服务（持有 Hub，按需解析每游客的世界）。
type Server struct {
	hub    *Hub
	reg    *life.Registry // 跨进程 Travel 的 WorldKey→Endpoint 寻址（发起 Travel 时按 WorldKey 寻址）
	mux    *http.ServeMux
}

// NewServer 创建服务。
func NewServer(hub *Hub, reg *life.Registry) *Server {
	s := &Server{hub: hub, reg: reg, mux: http.NewServeMux()}
	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/agents/", s.handleAgent)
	s.mux.HandleFunc("/api/events", s.handleEvents)
	s.mux.HandleFunc("/api/threads", s.handleThreads)
	s.mux.HandleFunc("/api/threads/", s.handleThreadDecide)
	s.mux.HandleFunc("/api/speed", s.handleSpeed)
	s.mux.HandleFunc("/api/whylog", s.handleWhyLog)
	s.mux.HandleFunc("/api/stream", s.handleStream)
	// 跨进程 Life 端点：Economy（或其他世界）POST /life/enter 把 Agent 落入本游客的世界。
	s.mux.HandleFunc("/life/enter", s.handleLifeEnter)
	// 跨进程事件端点：Economy 把“Marcus 完成工作”等事件推回 Village 事件流（departure/work/return 三段可见）。
	s.mux.HandleFunc("/life/event", s.handleLifeEvent)
	// 手动触发跨世界旅行：强制把演示世界的铁匠 John 送往 Economy（忽略技能门槛，便于演示/重放）。
	s.mux.HandleFunc("/api/trigger-travel", s.handleTriggerTravel)
	s.mux.HandleFunc("/", s.serveWeb)
	return s
}

func (s *Server) Start(addr string) error {
	log.Printf("[village] 村庄服务监听 %s", addr)
	srv := &http.Server{Addr: addr, Handler: s.mux}
	return srv.ListenAndServe()
}

// inst 解析当前游客身份并返回其世界实例（按需惰性创建）。失败时写错误并返回 false。
func (s *Server) inst(r *http.Request, w http.ResponseWriter) (*Instance, bool) {
	uid := s.ensureUID(r, w)
	inst, err := s.hub.Get(uid)
	if err != nil {
		http.Error(w, "world init failed: "+err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return inst, true
}

// ensureUID 从 cookie 读取游客身份；不存在则生成并写回（同源请求浏览器自动带 cookie）。
func (s *Server) ensureUID(r *http.Request, w http.ResponseWriter) string {
	// 演示模式：所有 web 访客共享同一个演示世界（含铁匠 Marcus），便于展示跨进程旅程。
	if u := demoUID(); u != "" {
		http.SetCookie(w, &http.Cookie{
			Name: "wc_uid", Value: u, Path: "/", MaxAge: 86400 * 365,
			HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		return u
	}
	if c, err := r.Cookie("wc_uid"); err == nil && c.Value != "" {
		return c.Value
	}
	uid := randUID()
	http.SetCookie(w, &http.Cookie{
		Name: "wc_uid", Value: uid, Path: "/", MaxAge: 86400 * 365,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	return uid
}

// ---- 静态前端 ----

var webFS = func() fs.FS {
	sub, err := fs.Sub(webstatic.DistFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}()

func (s *Server) serveWeb(w http.ResponseWriter, r *http.Request) {
	s.ensureUID(r, w) // 首次访问即写入身份 cookie，保证后续 API/SSE 同源共享
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" || p == "." || p == "\\" {
		p = "index.html"
	}
	if data, err := fs.ReadFile(webFS, p); err == nil {
		w.Header().Set("Content-Type", contentType(p))
		_, _ = w.Write(data)
		return
	}
	if index, err := fs.ReadFile(webFS, "index.html"); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
		return
	}
	http.NotFound(w, r)
}

func contentType(p string) string {
	switch path.Ext(p) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".woff", ".woff2":
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}

// ---- REST ----

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.inst(r, w)
	if !ok {
		return
	}
	writeJSON(w, inst.Mod.Game().State())
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.inst(r, w)
	if !ok {
		return
	}
	idStr := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agents/"), "/")
	if idStr == "" {
		http.Error(w, "bad agent id", http.StatusBadRequest)
		return
	}
	parts := strings.Split(idStr, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad agent id", http.StatusBadRequest)
		return
	}
	// 子路径：chat / influence / gift / borrow
	if len(parts) >= 2 {
		s.handleAgentAction(w, r, inst, id, parts[1])
		return
	}
	detail := inst.Mod.Game().AgentDetail(id)
	if detail == nil {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	writeJSON(w, detail)
}

func (s *Server) handleAgentAction(w http.ResponseWriter, r *http.Request, inst *Instance, id int64, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var resp map[string]interface{}
	switch action {
	case "chat":
		msg, _ := body["message"].(string)
		resp = inst.Mod.Game().Chat(id, msg)
	case "influence":
		advice, _ := body["advice"].(string)
		resp = inst.Mod.Game().Influence(id, advice)
	case "gift":
		amount := toInt64(body["gold"])
		resp = inst.Mod.Game().Gift(id, amount)
	case "borrow":
		amount := toInt64(body["gold"])
		resp = inst.Mod.Game().Borrow(id, amount)
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	if denied, ok := resp["error"].(string); ok && denied == "daily_limit" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	if resp == nil {
		writeJSON(w, map[string]interface{}{"error": "invalid agent or request"})
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.inst(r, w)
	if !ok {
		return
	}
	day, _ := strconv.Atoi(r.URL.Query().Get("day"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if r.URL.Query().Get("day") == "" {
		writeJSON(w, map[string]interface{}{"day": inst.Mod.Game().Day(), "events": inst.Mod.Game().AllEvents(limit)})
		return
	}
	writeJSON(w, map[string]interface{}{"day": day, "events": inst.Mod.Game().EventsByDay(day, limit)})
}

// handleThreads 悬念线程列表（未决的在前，已了结的在后）。
func (s *Server) handleThreads(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.inst(r, w)
	if !ok {
		return
	}
	writeJSON(w, map[string]interface{}{"threads": inst.Mod.Game().Threads()})
}

// handleThreadDecide 玩家对一条悬念做出选择：POST /api/threads/{id}/decide {choice}
func (s *Server) handleThreadDecide(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.inst(r, w)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/threads/"), "/"), "/")
	if len(parts) < 2 || parts[1] != "decide" {
		http.Error(w, "bad thread request", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad thread id", http.StatusBadRequest)
		return
	}
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	choice, _ := body["choice"].(string)
	resp := inst.Mod.Game().DecideThread(id, choice)
	if denied, ok := resp["error"].(string); ok && denied == "daily_limit" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	if resp == nil {
		http.Error(w, "invalid thread or choice", http.StatusBadRequest)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) handleSpeed(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.inst(r, w)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	inst.Mod.Game().SetSpeed(int(toInt64(body["speed"])))
	writeJSON(w, map[string]interface{}{"speed": inst.Mod.Game().Speed()})
}

// handleWhyLog 返回 Village 的 Causal Life Log（重要决定的结构化因果记录）。
// 可选 ?days=N 只取最近 N 天，用于"过去 7 天发生了哪些重要决定、为什么"。
//
//	GET /api/whylog        → 全部 WhyLog
//	GET /api/whylog?days=7 → 最近 7 天
func (s *Server) handleWhyLog(w http.ResponseWriter, r *http.Request) {
	inst := s.hub.PrimaryInstance()
	if inst == nil {
		http.Error(w, "no demo world", http.StatusNotFound)
		return
	}
	if d := r.URL.Query().Get("days"); d != "" {
		if n, e := strconv.Atoi(d); e == nil && n > 0 {
			writeJSON(w, map[string]interface{}{"day": inst.Mod.Game().Day(), "whylog": inst.Mod.Game().WhyLogLast(n)})
			return
		}
	}
	writeJSON(w, map[string]interface{}{"day": inst.Mod.Game().Day(), "whylog": inst.Mod.Game().WhyLog()})
}

// handleLifeEnter 跨进程 Travel 的落地入口：Economy 把 Marcus 的 AgentPortable POST 到这里，
// 由本游客世界实例的 adapter 落地（Enter）。uid 默认取当前访客；跨进程回村时由发起方用 ?uid= 指定。
// 注意：Village 不调用 Economy 代码，Economy 也不调用 Village 代码——只有 Life Runtime 经 HTTP 往来。
func (s *Server) handleLifeEnter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		uid = s.hub.DefaultUID() // 跨进程回村：默认落入演示世界
	}
	if uid == "" {
		uid = s.ensureUID(r, w) // 仍未指定则落入当前访客世界
	}
	inst, err := s.hub.Get(uid)
	if err != nil {
		http.Error(w, "world init failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var p life.AgentPortable
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad portable: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := inst.Adapter.Enter(p); err != nil {
		http.Error(w, "enter failed: "+err.Error(), http.StatusConflict)
		return
	}
	// 跨进程旅程“return”：Marcus 从经济世界归来，村庄事件流可见。
	inst.World.EmitEvent("return", "🏡", p.Identity.Name, "returns from the Economy world, with newfound skill", "brought back cross-world work experience")
	writeJSON(w, map[string]interface{}{"ok": true, "world": "village", "uid": uid})
}

// handleLifeEvent 接收来自其他世界（如 Economy）推送的事件，并写入本世界（演示世界）事件流，
// 使 Village/Event Stream 能看到 departure / work / return 三段旅程。它不携带任何世界特定逻辑。
func (s *Server) handleLifeEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var ev life.PortableEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "bad event: "+err.Error(), http.StatusBadRequest)
		return
	}
	uid := s.hub.DefaultUID()
	if uid == "" {
		if p := s.hub.PrimaryInstance(); p != nil {
			uid = p.UID
		}
	}
	if uid == "" {
		http.Error(w, "no demo world", http.StatusNotFound)
		return
	}
	inst, err := s.hub.Get(uid)
	if err != nil {
		http.Error(w, "world init failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	inst.World.EmitEvent(ev.Type, ev.Icon, ev.Actor, ev.Text, ev.Why)
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handleTriggerTravel 手动把演示世界的铁匠 John 送往 Economy（忽略技能门槛）。
// 自动扫描会因为 John 技能已达标而不再出发；此接口用于演示/重放整段旅程。
func (s *Server) handleTriggerTravel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	uid := s.hub.DefaultUID()
	if uid == "" {
		uid = s.ensureUID(r, w)
	}
	inst, err := s.hub.Get(uid)
	if err != nil {
		http.Error(w, "world init failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	local, ok := inst.World.FindAgentByName("John") // 铁匠 Marcus
	if !ok {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "未找到铁匠 John"})
		return
	}
	stable := life.StableID("village", local)
	if _, ok := inst.Adapter.LocalID(stable); !ok {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "John 不在村庄（可能已在旅途中）"})
		return
	}
	who := "John"
	if raw, ok := inst.Adapter.StoreGet(local); ok {
		if a, ok := raw.(*village.Agent); ok && a.Name != "" {
			who = a.Name
		}
	}
	ecoURL, ok := s.reg.Endpoint("economy")
	if !ok {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "economy 端点未配置（检查 ECO_LIFE_URL）"})
		return
	}
	tr, err := life.MoveRemote(inst.Adapter, ecoURL, stable)
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	inst.World.EmitEvent("departure", "🚀", who, "Blacksmith "+who+" leaves the village for the Economy world", "manual trigger")
	log.Printf("[village] 手动触发 Marcus 出发去经济世界（%s）", tr.Status)
	writeJSON(w, map[string]interface{}{"ok": true, "status": tr.Status})
}

// handleStream SSE 实时流（village.state 每 tick 全量快照 + village.event 故事事件）。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.inst(r, w)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher.Flush()

	obs := inst.World.Obs()
	if obs == nil {
		return
	}
	_, ch, cancel := obs.Subscribe(64)
	defer cancel()
	if ch == nil {
		http.Error(w, "subscription limit reached, retry later", http.StatusServiceUnavailable)
		return
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", ev.Encode())
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(v)
}

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}
