package realworld

import (
	"time"

	"agentworld/internal/models"
)

// Seed 幂等注入 10 个真实任务 + 一个具备全部技能的 worker Agent。
// 第一个真实任务闭环就是从这 10 个里跑出来的。
func Seed(s *Service) error {
	d := s.DB
	// 幂等：已 seed 过则跳过
	var existing int64
	d.Model(&models.RealWorldJob{}).Where("customer = ?", "seed-batch-1").Count(&existing)
	if existing > 0 {
		return nil
	}

	// 确保 worker Agent（真实结算与模拟经济隔离，但执行需要实体 Agent）
	var agent models.Agent
	err := d.Where("name = ? AND world = ?", "RWX-Atlas", "realworld").First(&agent).Error
	if err != nil {
		agent = models.Agent{
			Name:    "RWX-Atlas",
			World:   "realworld",
			Kind:    "ai",
			Status:  "active",
			Goal:    "完成真实世界付费任务并交付可验证的工件",
			UseLLM:  false,
		}
		if err := d.Create(&agent).Error; err != nil {
			return err
		}
	}

	// 赋予全部所需技能
	skills := []string{
		"csv-postgres", "csv-clean", "json-csv", "sql-report", "api-integration",
		"md-report", "python-data", "api-docs", "unit-tests", "bugfix",
	}
	for _, sk := range skills {
		var c int64
		d.Model(&models.AgentCapability{}).Where("agent_id = ? AND skill = ?", agent.ID, sk).Count(&c)
		if c == 0 {
			d.Create(&models.AgentCapability{AgentID: agent.ID, World: "realworld", Skill: sk, Description: "seed"})
		}
	}

	deadline := time.Now().Add(7 * 24 * time.Hour)
	jobs := []CreateJobInput{
		{Title: "CSV → PostgreSQL", Description: "将 users.csv 导入 PostgreSQL，产出 schema.sql 与导入脚本。", Reward: 5, RequiredSkill: "csv-postgres", AcceptanceCriteria: "自动测试：脚本跑通且表已建、行数正确", Customer: "seed-batch-1"},
		{Title: "CSV 数据清洗", Description: "清洗脏数据：去重 + 补齐缺失必填字段。", Reward: 3, RequiredSkill: "csv-clean", AcceptanceCriteria: "行数/字段测试", Customer: "seed-batch-1"},
		{Title: "JSON → CSV", Description: "把嵌套 JSON 转成扁平 CSV。", Reward: 2, RequiredSkill: "json-csv", AcceptanceCriteria: "Schema 校验", Customer: "seed-batch-1"},
		{Title: "PostgreSQL 报表 SQL", Description: "按区域/客户聚合营收的报表查询。", Reward: 5, RequiredSkill: "sql-report", AcceptanceCriteria: "执行 SQL 得到聚合结果", Customer: "seed-batch-1"},
		{Title: "REST API 联调脚本", Description: "可离线的 REST API 集成测试脚本。", Reward: 5, RequiredSkill: "api-integration", AcceptanceCriteria: "Integration Test 通过", Customer: "seed-batch-1"},
		{Title: "自动生成 Markdown 报告", Description: "根据指标数据生成业务摘要报告。", Reward: 3, RequiredSkill: "md-report", AcceptanceCriteria: "文件检查", Customer: "seed-batch-1"},
		{Title: "Python 数据处理脚本", Description: "对成绩数据做统计并输出结果。", Reward: 5, RequiredSkill: "python-data", AcceptanceCriteria: "Test Suite 通过", Customer: "seed-batch-1"},
		{Title: "API 文档生成", Description: "从 API spec 生成 Markdown 文档。", Reward: 2, RequiredSkill: "api-docs", AcceptanceCriteria: "人工检查文档完整性", Customer: "seed-batch-1"},
		{Title: "单元测试补全", Description: "为 mathlib 补全单元测试。", Reward: 5, RequiredSkill: "unit-tests", AcceptanceCriteria: "Test Pass", Customer: "seed-batch-1"},
		{Title: "小型 Bug Fix", Description: "修复 running_total 的边界问题并补回归测试。", Reward: 10, RequiredSkill: "bugfix", AcceptanceCriteria: "Regression Test 通过", Customer: "seed-batch-1"},
	}
	for _, j := range jobs {
		j.Deadline = &deadline
		if _, err := s.CreateJob(j); err != nil {
			return err
		}
	}
	return nil
}
