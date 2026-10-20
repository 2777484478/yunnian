package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/model"
)

// DeployRiskDTO 一次部署的**风险分**。
//
// 这份分数的价值不在数字本身，而在 `Reasons` —— 每一分都要能指向一个
// 可核对的事实（"近 30 次错了 7 次"而不是"模型觉得很危险"）。
// 所以它是**纯计算**，不调模型：模型只会把这些计数编成一段话，
// 而算出来的数可复现、能对账，出问题能指着说原因。
type DeployRiskDTO struct {
	// Score 0-100，越大越危险
	Score int `json:"score"`
	// Level low / medium / high —— 前端按它决定"要不要二次确认"
	Level string `json:"level"`
	// Reasons 逐条说明这分是怎么来的（空数组 = 没有任何风险因子命中）
	Reasons []string `json:"reasons"`
}

// riskFactor 一个风险因子：命中就加 weight 分，并把说明写进 reasons。
type riskFactor struct {
	weight int
	reason string
	hit    bool
}

// DeployRisk 算一次部署的风险分。
//
// 五个因子全部是**确定性计数**，没有一个是猜的：
//  1. 该服务在**该环境**近 30 次的失败率（最重的权重：它直接反映"这块地好不好种"）
//  2. 该服务历史上回滚过几次（回滚意味着"发上去才发现不对"，是失败之外的另一种信号）
//  3. 距上次部署的间隔（10 分钟内又发 = 高频变更 / 可能在试错）
//  4. 目标环境是不是正式区
//  5. 同环境当前是否有别的部署在飞（并发会互相抢资源、也会让排查变乱）
//
// ⚠️ 刻意**不看**镜像 tag、commit 内容、代码改动量 —— 平台拿不到可靠的增量信息，
// 用 tag 名字或 commit 数量猜"改动大不大"属于编数据。
func (s *CDService) DeployRisk(ctx context.Context, projectKey, envKey string) (*DeployRiskDTO, error) {
	pk := strings.TrimSpace(projectKey)
	ek := strings.TrimSpace(envKey)
	if pk == "" || ek == "" {
		return nil, errs.Invalid("需要项目与环境才能评估风险")
	}

	var factors []riskFactor
	/** 该服务在该环境的近年失败率（用于下面的高危硬规则） */
	worstFailRate := 0.0

	// ① 近 30 次失败率（权重 40）
	var recent []model.DeployRecord
	if err := s.db(ctx).
		Where("project_key = ? AND env_key = ?", pk, ek).
		Order("id DESC").Limit(30).Find(&recent).Error; err != nil {
		return nil, errs.Internalf(err, "读取部署历史失败")
	}
	if n := len(recent); n >= 3 { // 少于 3 次谈"失败率"没有意义
		bad := 0
		for _, r := range recent {
			// interrupted 也算：它同样意味着"这次没发成"
			if r.Status == "failed" || r.Status == "interrupted" {
				bad++
			}
		}
		pct := float64(bad) / float64(n)
		worstFailRate = pct
		if pct >= 0.2 {
			factors = append(factors, riskFactor{
				weight: int(pct * 40),
				reason: fmt.Sprintf("该服务在此环境近 %d 次部署中有 %d 次失败（%.0f%%）", n, bad, pct*100),
				hit:    true,
			})
		}
	}

	// ② 历史回滚次数（权重 15，最多算 3 次）
	var rollbacks int64
	if err := s.db(ctx).Model(&model.RollbackRecord{}).
		Where("project_key = ?", pk).Count(&rollbacks).Error; err == nil && rollbacks > 0 {
		w := int(rollbacks)
		if w > 3 {
			w = 3
		}
		factors = append(factors, riskFactor{
			weight: w * 5,
			reason: fmt.Sprintf("该服务历史上回滚过 %d 次", rollbacks),
			hit:    true,
		})
	}

	// ③ 距上次部署的间隔（权重 15）
	var last model.DeployRecord
	if err := s.db(ctx).Where("project_key = ? AND env_key = ?", pk, ek).
		Order("id DESC").First(&last).Error; err == nil && last.CreatedAt != (time.Time{}) {
		if gap := time.Since(last.CreatedAt); gap > 0 && gap < 10*time.Minute {
			factors = append(factors, riskFactor{
				weight: 15,
				reason: fmt.Sprintf("%s前刚部署过一次（10 分钟内重复发布）", humanGap(gap)),
				hit:    true,
			})
		}
	}

	// ④ 正式区（权重 15）
	var env model.Environment
	if err := s.db(ctx).Where("env_key = ?", ek).First(&env).Error; err == nil {
		if strings.EqualFold(env.EnvKey, "prod") || strings.Contains(env.Name, "生产") {
			w := 15
			if env.NeedApproval {
				w = 8 // 要审批的环境本身已有闸门，风险权重降一档
			}
			factors = append(factors, riskFactor{
				weight: w,
				reason: fmt.Sprintf("目标是正式区（%s）", env.Name),
				hit:    true,
			})
		}
	}

	// ⑤ 同环境有在飞部署（权重 15）
	var inflight int64
	if err := s.db(ctx).Model(&model.DeployRecord{}).
		Where("env_key = ? AND status IN ?", ek, []string{"queued", "progressing", "pending_approval"}).
		Count(&inflight).Error; err == nil && inflight > 0 {
		factors = append(factors, riskFactor{
			weight: 15,
			reason: fmt.Sprintf("该环境当前还有 %d 个部署在排队或进行中", inflight),
			hit:    true,
		})
	}

	score := 0
	reasons := make([]string, 0, len(factors))
	for _, f := range factors {
		if !f.hit {
			continue
		}
		score += f.weight
		reasons = append(reasons, f.reason)
	}
	if score > 100 {
		score = 100
	}

	level := "low"
	switch {
	case score >= 55:
		level = "high"
	case worstFailRate >= 0.5:
		// ⚠️ **硬规则**：一半以上的发布都失败，不管总分多少都算高危。
		// 实测 `aiobasic@test` 是 10 次错 7 次（70%），而按加权分数只到 38 分 ——
		// 那个分值会让人以为"还好"，但"这地方发十次错七次"显然不是"还好"。
		level = "high"
	case score >= 25:
		level = "medium"
	}
	return &DeployRiskDTO{Score: score, Level: level, Reasons: reasons}, nil
}

// humanGap 把时长写成"x 分钟"这种人话。
func humanGap(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%d 小时", int(d.Hours()))
}
