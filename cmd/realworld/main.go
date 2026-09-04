package main

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentworld/internal/config"
	"agentworld/internal/db"
	"agentworld/internal/llm"
	"agentworld/internal/realworld"
)

func main() {
	driver := envOr("DB_DRIVER", config.C.DBDriver)
	dsn := envOr("DB_DSN", config.C.DBDSN)
	wsRoot := envOr("RW_WORKSPACE", "realworld_workspaces")
	port := envOr("RW_PORT", "8099")
	auto := envOr("RW_AUTO", "true") == "true"

	d, err := db.Open(driver, dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}

	llmClient := llm.New(config.C.LLMBase, config.C.LLMKey, config.C.LLMModel)
	if llmClient.Enabled() {
		log.Printf("已启用真实 LLM: %s", llmClient.ModelName())
	}

	svc := realworld.New(d)
	svc.LLM = llmClient
	svc.WorkspaceRoot = wsRoot

	if err := realworld.Seed(svc); err != nil {
		log.Fatalf("seed: %v", err)
	}
	log.Printf("M8 Real Economy 已就绪；workspace=%s", wsRoot)

	// 一次性批处理模式：seed + 接单 + 真实执行所有 OPEN 任务后退出（不启动服务）。
	// 用于演示/验证闭环，也适合 CI。RW_ONESHOT=true ./rw
	if envOr("RW_ONESHOT", "") == "true" {
		jobs, _ := svc.ListJobs(realworld.StatusOpen)
		for _, j := range jobs {
			if err := svc.AcceptJob(j.ID, 0); err != nil {
				log.Printf("[oneshot] accept %d: %v", j.ID, err)
				continue
			}
			if err := svc.ExecuteJob(j.ID); err != nil {
				log.Printf("[oneshot] execute %d: %v", j.ID, err)
				continue
			}
		}
		done, _ := svc.ListJobs("")
		for _, j := range done {
			log.Printf("[oneshot] job %d %q -> %s ($%.2f %s)", j.ID, j.Title, j.Status, j.Reward, j.Currency)
		}
		log.Println("[oneshot] 完成，退出（未启动服务）")
		return
	}

	// 自动接单 + 执行循环：Agent 自主接 OPEN 任务并真实执行，停在 SUBMITTED 等待人工验收/支付。
	if auto {
		go func() {
			for {
				jobs, err := svc.ListJobs(realworld.StatusOpen)
				if err == nil {
					for _, j := range jobs {
						if err := svc.AcceptJob(j.ID, 0); err != nil {
							log.Printf("[auto] accept %d: %v", j.ID, err)
							continue
						}
						if err := svc.ExecuteJob(j.ID); err != nil {
							log.Printf("[auto] execute %d: %v", j.ID, err)
							continue
						}
						log.Printf("[auto] job %d 执行完成 -> SUBMITTED", j.ID)
					}
				}
				time.Sleep(5 * time.Second)
			}
		}()
	}

	http.HandleFunc("/", dashboardHandler(svc))
	http.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		jobs, _ := svc.ListJobs("")
		writeJSON(w, jobs)
	})
	http.HandleFunc("/api/jobs/", jobHandler(svc))
	http.HandleFunc("/ws/", workspaceFileHandler(svc))

	addr := ":" + port
	log.Printf("M8 dashboard: http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func jobHandler(svc *realworld.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// /api/jobs/{id}[/action]
		rest := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
		parts := strings.Split(rest, "/")
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			http.Error(w, "bad id", 400)
			return
		}
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}

		if r.Method == http.MethodGet && action == "" {
			job, err := svc.GetJob(id)
			if err != nil {
				http.Error(w, "not found", 404)
				return
			}
			exec, _ := svc.GetExecution(id)
			writeJSON(w, map[string]interface{}{"job": job, "execution": exec})
			return
		}

		var err2 error
		switch action {
		case "accept":
			err2 = svc.AcceptJob(id, 0)
		case "execute":
			err2 = svc.ExecuteJob(id)
		case "approve":
			err2 = svc.ApproveJob(id)
		case "reject":
			err2 = svc.RejectJob(id, r.FormValue("note"))
		case "pay":
			err2 = svc.PayJob(id, r.FormValue("provider"), r.FormValue("txid"))
		default:
			http.Error(w, "unknown action", 400)
			return
		}
		if err2 != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err2.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "job_id": id, "action": action})
	}
}

// workspaceFileHandler 暴露某个任务 workspace 里的真实交付文件：/ws/job-{id}/?name=import.py
func workspaceFileHandler(svc *realworld.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/ws/")
		// 形如 job-3/import.py
		idx := strings.Index(rest, "/")
		if idx < 0 {
			http.Error(w, "usage: /ws/job-{id}/?name=file", 400)
			return
		}
		prefix := rest[:idx]             // job-3
		name := r.URL.Query().Get("name") // import.py
		if !strings.HasPrefix(prefix, "job-") || name == "" {
			http.Error(w, "bad request", 400)
			return
		}
		jobID, err := strconv.ParseInt(strings.TrimPrefix(prefix, "job-"), 10, 64)
		if err != nil {
			http.Error(w, "bad job id", 400)
			return
		}
		exec, err := svc.GetExecution(jobID)
		if err != nil || exec == nil || exec.Workspace == "" {
			http.Error(w, "no workspace", 404)
			return
		}
		// 防目录穿越
		clean := filepath.Clean("/" + name)
		full := filepath.Join(exec.Workspace, clean[1:])
		data, err := os.ReadFile(full)
		if err != nil {
			http.Error(w, "file not found", 404)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(data)
	}
}

func dashboardHandler(svc *realworld.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		jobs, _ := svc.ListJobs("")
		var b strings.Builder
		b.WriteString(`<html><head><meta charset="utf-8"><title>M8 Real Economy</title>
<style>body{font-family:system-ui;margin:24px;background:#0f1115;color:#e6e6e6}
.card{background:#1a1d24;border:1px solid #2a2f3a;border-radius:10px;padding:14px;margin:12px 0}
.status{font-weight:700} .muted{color:#8b93a3;font-size:12px}
button{background:#2d6cdf;color:#fff;border:0;border-radius:6px;padding:6px 10px;margin:2px;cursor:pointer}
form{display:inline} code{background:#0f1115;padding:1px 4px;border-radius:3px}</style></head><body>`)
		b.WriteString("<h1>M8 Real Economy</h1>")
		b.WriteString(fmt.Sprintf("<p class='muted'>%d 个任务 · 真实结算与模拟经济隔离</p>", len(jobs)))
		for _, j := range jobs {
			exec, _ := svc.GetExecution(j.ID)
			agent := ""
			if exec != nil {
				agent = exec.AgentName
			}
			b.WriteString("<div class='card'>")
			b.WriteString(fmt.Sprintf("<div><b>#%d %s</b> · <span class='status'>%s</span></div>", j.ID, html.EscapeString(j.Title), j.Status))
			b.WriteString(fmt.Sprintf("<div class='muted'>skill=%s · reward=$%.2f %s · customer=%s · agent=%s</div>",
				html.EscapeString(j.RequiredSkill), j.Reward, j.Currency, html.EscapeString(j.Customer), html.EscapeString(agent)))
			b.WriteString(fmt.Sprintf("<div class='muted'>验收: %s</div>", html.EscapeString(j.AcceptanceCriteria)))
			if exec != nil && exec.Workspace != "" {
				b.WriteString(fmt.Sprintf("<div class='muted'>workspace: <code>%s</code> · success=%v</div>", html.EscapeString(exec.Workspace), exec.Success))
				for _, f := range exec.OutputFiles {
					b.WriteString(fmt.Sprintf(" <a href='/ws/job-%d/?name=%s'>%s</a>", j.ID, f, f))
				}
			}
			// 动作按钮
			b.WriteString("<div style='margin-top:8px'>")
			if j.Status == realworld.StatusOpen {
				b.WriteString(form(j.ID, "accept", "接单+执行", ""))
			}
			if j.Status == realworld.StatusSubmitted {
				b.WriteString(form(j.ID, "approve", "验收通过", ""))
				b.WriteString(form(j.ID, "reject", "拒绝", "note"))
			}
			if j.Status == realworld.StatusRejected {
				b.WriteString(form(j.ID, "execute", "重做", ""))
			}
			if j.Status == realworld.StatusApproved {
				b.WriteString(form(j.ID, "pay", "支付", "provider,txid"))
			}
			b.WriteString("</div>")
			b.WriteString("</div>")
		}
		b.WriteString("</body></html>")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(b.String()))
	}
}

func form(jobID int64, action, label, extraFields string) string {
	s := fmt.Sprintf("<form method='post' action='/api/jobs/%d/%s'>", jobID, action)
	if extraFields == "note" {
		s += "<input name='note' placeholder='拒绝原因' size='12'>"
	}
	if extraFields == "provider,txid" {
		s += "<input name='provider' placeholder='渠道' size='8'><input name='txid' placeholder='交易号' size='10'>"
	}
	s += fmt.Sprintf("<button>%s</button></form>", label)
	return s
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
