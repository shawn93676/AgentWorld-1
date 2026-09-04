package realworld

import (
	"errors"
	"fmt"
	"time"

	"agentworld/internal/llm"
	"agentworld/internal/models"

	"gorm.io/gorm"
)

// 状态机：OPEN → ACCEPTED → EXECUTING → SUBMITTED → APPROVED → PAID，SUBMITTED 可 REJECTED 回到 EXECUTING 重做。
const (
	StatusOpen      = "OPEN"
	StatusAccepted  = "ACCEPTED"
	StatusExecuting = "EXECUTING"
	StatusSubmitted = "SUBMITTED"
	StatusApproved  = "APPROVED"
	StatusRejected  = "REJECTED"
	StatusPaid      = "PAID"
)

var transitions = map[string][]string{
	StatusOpen:      {StatusAccepted},
	StatusAccepted:  {StatusExecuting},
	StatusExecuting: {StatusSubmitted},
	StatusSubmitted: {StatusApproved, StatusRejected},
	StatusApproved:  {StatusPaid},
	StatusRejected:  {StatusExecuting}, // 重做
	StatusPaid:      {},
}

func canTransition(from, to string) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Service 封装 Real Economy 的领域逻辑与状态机。
type Service struct {
	DB             *gorm.DB
	LLM            *llm.Client
	WorkspaceRoot  string
	AutoRunEnabled bool
}

func New(db *gorm.DB) *Service {
	return &Service{DB: db}
}

// --- 查询 ---

func (s *Service) GetJob(id int64) (*models.RealWorldJob, error) {
	var j models.RealWorldJob
	if err := s.DB.First(&j, id).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *Service) ListJobs(status string) ([]models.RealWorldJob, error) {
	var jobs []models.RealWorldJob
	q := s.DB.Order("id desc")
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if err := q.Find(&jobs).Error; err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Service) GetExecution(jobID int64) (*models.RealWorldExecution, error) {
	var e models.RealWorldExecution
	if err := s.DB.First(&e, "job_id = ?", jobID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &e, nil
}

// --- 状态流转 ---

type CreateJobInput struct {
	Title              string
	Description        string
	Reward             float64
	Currency           string
	Deadline           *time.Time
	RequiredSkill      string
	AcceptanceCriteria string
	Customer           string
}

func (s *Service) CreateJob(in CreateJobInput) (*models.RealWorldJob, error) {
	j := models.RealWorldJob{
		Title:              in.Title,
		Description:        in.Description,
		Reward:             in.Reward,
		Currency:           orDefault(in.Currency, "USD"),
		Deadline:           in.Deadline,
		RequiredSkill:      in.RequiredSkill,
		AcceptanceCriteria: in.AcceptanceCriteria,
		Status:             StatusOpen,
		Customer:           in.Customer,
		PaymentStatus:      "unpaid",
	}
	if err := s.DB.Create(&j).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

// AcceptJob Agent 自主接单：OPEN → ACCEPTED，并按技能匹配合适的 Agent。
// agentID>0 时直接使用指定 Agent；否则按 RequiredSkill 匹配，兜底任意 AI Agent。
func (s *Service) AcceptJob(jobID, agentID int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var j models.RealWorldJob
		if err := tx.First(&j, jobID).Error; err != nil {
			return err
		}
		if !canTransition(j.Status, StatusAccepted) {
			return fmt.Errorf("job %d 状态 %s 不可接单", jobID, j.Status)
		}
		agent, err := s.pickAgent(tx, agentID, j.RequiredSkill)
		if err != nil {
			return err
		}
		// 预建执行记录（先记 Agent，执行时补全 workspace/日志）
		exec := models.RealWorldExecution{JobID: jobID, AgentID: agent.ID, AgentName: agent.Name}
		if err := tx.Save(&exec).Error; err != nil {
			return err
		}
		j.Status = StatusAccepted
		return tx.Save(&j).Error
	})
}

func (s *Service) pickAgent(tx *gorm.DB, agentID int64, skill string) (models.Agent, error) {
	if agentID > 0 {
		var a models.Agent
		if err := tx.First(&a, agentID).Error; err == nil {
			return a, nil
		}
	}
	// 按技能匹配
	var cap models.AgentCapability
	if err := tx.Where("skill = ?", skill).First(&cap).Error; err == nil {
		var a models.Agent
		if tx.First(&a, cap.AgentID).Error == nil {
			return a, nil
		}
	}
	// 兜底：任意 ai agent
	var a models.Agent
	if tx.Where("kind = ?", "ai").First(&a).Error == nil {
		return a, nil
	}
	return models.Agent{}, fmt.Errorf("没有可用 Agent：请先 seed（确保存在具备技能 %q 的 Agent）", skill)
}

// ExecuteJob 真实执行：ACCEPTED → EXECUTING →（跑执行器）→ SUBMITTED。
func (s *Service) ExecuteJob(jobID int64) error {
	var j models.RealWorldJob
	if err := s.DB.First(&j, jobID).Error; err != nil {
		return err
	}
	if !canTransition(j.Status, StatusExecuting) {
		return fmt.Errorf("job %d 状态 %s 不可执行", jobID, j.Status)
	}
	var exec models.RealWorldExecution
	if err := s.DB.First(&exec, "job_id = ?", jobID).Error; err != nil {
		return err
	}
	var agent models.Agent
	s.DB.First(&agent, exec.AgentID)

	// 进入执行中
	j.Status = StatusExecuting
	if err := s.DB.Save(&j).Error; err != nil {
		return err
	}

	ex := NewExecutor(s.LLM, s.WorkspaceRoot)
	result, err := ex.Run(j, agent)
	if err != nil {
		// 执行失败：保留在 EXECUTING 以便重做，并落盘失败日志（不再前进到 SUBMITTED）
		exec.Workspace = result.Workspace
		exec.Logs = result.Logs + "\n[ERROR] " + err.Error()
		exec.Success = false
		if result.Result != "" {
			exec.Result = result.Result
		}
		s.DB.Save(&exec)
		j.Status = StatusExecuting
		s.DB.Save(&j)
		return err
	}

	// 合并执行结果
	exec.Workspace = result.Workspace
	exec.StartedAt = result.StartedAt
	exec.FinishedAt = result.FinishedAt
	exec.OutputFiles = result.OutputFiles
	exec.Logs = result.Logs
	exec.Result = result.Result
	exec.Success = result.Success
	if err := s.DB.Save(&exec).Error; err != nil {
		return err
	}

	j.Status = StatusSubmitted
	return s.DB.Save(&j).Error
}

// ApproveJob 验收通过：SUBMITTED → APPROVED。
func (s *Service) ApproveJob(jobID int64) error {
	return s.transition(jobID, StatusApproved)
}

// RejectJob 验收不通过：SUBMITTED → REJECTED（可重做）。
func (s *Service) RejectJob(jobID int64, note string) error {
	// 把拒绝原因追加到执行日志，便于重做时参考
	if note != "" {
		if e, err := s.GetExecution(jobID); err == nil && e != nil {
			e.Logs += "\n[REJECT] " + note
			s.DB.Save(e)
		}
	}
	return s.transition(jobID, StatusRejected)
}

// PayJob 真实支付：APPROVED → PAID，并写入收入账本 RealWorldPayment。
// 这一步是真实结算的唯一入口，与任何模拟经济账户无关。
func (s *Service) PayJob(jobID int64, provider, txID string) error {
	var j models.RealWorldJob
	if err := s.DB.First(&j, jobID).Error; err != nil {
		return err
	}
	if !canTransition(j.Status, StatusPaid) {
		return fmt.Errorf("job %d 状态 %s 不可支付（需先 APPROVED）", jobID, j.Status)
	}
	pay := models.RealWorldPayment{
		JobID:         jobID,
		Amount:        j.Reward,
		Currency:      j.Currency,
		Provider:      orDefault(provider, "manual"),
		TransactionID: txID,
		Status:        "paid",
	}
	if err := s.DB.Create(&pay).Error; err != nil {
		return err
	}
	j.Status = StatusPaid
	j.PaymentStatus = "paid"
	return s.DB.Save(&j).Error
}

func (s *Service) transition(jobID int64, to string) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var j models.RealWorldJob
		if err := tx.First(&j, jobID).Error; err != nil {
			return err
		}
		if !canTransition(j.Status, to) {
			return fmt.Errorf("job %d 状态 %s 不可流转到 %s", jobID, j.Status, to)
		}
		j.Status = to
		return tx.Save(&j).Error
	})
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
