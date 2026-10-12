package store

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/yunnian/yunnian/internal/model"
)

// latestDeployStatus 根据服务当前状态推断「最近一次部署单」的状态，
// 使部署列表与拓扑页上的圆圈状态自洽（不会出现"服务在部署中但部署单已完成"）。
func (s *seeder) latestDeployStatus(envProj string) string {
	if v, ok := forcedServiceStatus[envProj]; ok {
		switch v {
		case "deploying":
			return "progressing"
		case "error":
			return "failed"
		case "pending":
			if envProj == "prod/user-service" {
				return "pending_approval"
			}
			return "queued"
		}
	}
	return "success"
}

// deploySteps 生成部署步骤明细。未完成的部署单后置步骤保持 pending，
// 前端据此渲染步骤条。
func deploySteps(deployID uint64, status string, startedAt time.Time, duration uint32) []model.DeployStep {
	names := []string{"拉取镜像", "更新工作负载", "健康检查", "就绪确认"}
	steps := make([]model.DeployStep, 0, len(names))

	perStep := time.Duration(duration) * time.Second / time.Duration(len(names))
	at := func(i int) *time.Time {
		t := startedAt.Add(time.Duration(i) * perStep)
		return &t
	}

	switch status {
	case "success":
		for i, n := range names {
			st := "success"
			detail := []string{
				"镜像拉取完成，共 5 层",
				"Deployment 已更新，滚动策略 RollingUpdate",
				"liveness/readiness 探针均通过",
				"全部副本就绪，流量已接入",
			}[i]
			steps = append(steps, model.DeployStep{
				DeployID: deployID, Seq: uint8(i + 1), Name: n,
				Status: st, Detail: detail, StartedAt: at(i),
			})
		}
	case "progressing":
		for i, n := range names {
			st, detail := "pending", ""
			switch {
			case i < 2:
				st = "success"
				detail = []string{"镜像拉取完成，共 5 层", "Deployment 已更新，滚动策略 RollingUpdate"}[i]
			case i == 2:
				st = "running"
				detail = "正在等待就绪：1/2 副本 Ready"
			}
			steps = append(steps, model.DeployStep{
				DeployID: deployID, Seq: uint8(i + 1), Name: n,
				Status: st, Detail: detail, StartedAt: at(i),
			})
		}
	case "failed":
		for i, n := range names {
			st, detail := "pending", ""
			switch {
			case i < 2:
				st = "success"
				detail = []string{"镜像拉取完成，共 5 层", "Deployment 已更新，滚动策略 RollingUpdate"}[i]
			case i == 2:
				st = "failed"
				detail = "就绪探针连续 3 次失败：CrashLoopBackOff"
			}
			steps = append(steps, model.DeployStep{
				DeployID: deployID, Seq: uint8(i + 1), Name: n,
				Status: st, Detail: detail, StartedAt: at(i),
			})
		}
	default: // pending_approval / queued / canceled
		for i, n := range names {
			steps = append(steps, model.DeployStep{
				DeployID: deployID, Seq: uint8(i + 1), Name: n, Status: "pending",
			})
		}
	}
	return steps
}

func (s *seeder) deploys(tx *gorm.DB) error {
	s.init()
	seq := 0

	for ei, es := range envSpecs {
		for pi, sp := range projectSpecs {
			envProj := es.Key + "/" + sp.Key
			svcID, ok := s.serviceIDs[envProj]
			if !ok {
				continue
			}
			imgID, hasImg := s.imageIDs[sp.Key]
			if !hasImg {
				continue // 没有镜像就无法产生部署单
			}

			count := 1 + s.rnd.Intn(3)
			for di := 0; di < count; di++ {
				seq++
				isLast := di == count-1
				ageHours := (count - di) * (4 + s.rnd.Intn(72))
				startedAt := s.now.Add(-time.Duration(ageHours) * time.Hour)

				status := "success"
				if isLast {
					status = s.latestDeployStatus(envProj)
				}

				duration := uint32(40 + s.rnd.Intn(160))
				replicas := targetReplicas(es.Key)
				operator := nameOfKey(sp.Key)
				operatorID := s.userIDs[operator]
				if operatorID == 0 {
					operatorID = s.ownerIDs[0]
				}

				rec := model.DeployRecord{
					DeployNo:     fmt.Sprintf("D%s%05d", startedAt.Format("20060102"), seq),
					EnvID:        s.envIDs[es.Key],
					EnvKey:       es.Key,
					ProjectID:    s.projectIDs[sp.Key],
					ProjectKey:   sp.Key,
					ServiceID:    svcID,
					ImageRecID:   imgID,
					Image:        s.imageRefs[sp.Key],
					Replicas:     replicas,
					Strategy:     "RollingUpdate",
					Status:       status,
					OperatorID:   operatorID,
					OperatorName: operator,
					CreatedAt:    startedAt,
					UpdatedAt:    startedAt,
				}

				switch status {
				case "success":
					finished := startedAt.Add(time.Duration(duration) * time.Second)
					rec.Progress = 100
					rec.StartedAt = &startedAt
					rec.FinishedAt = &finished
					rec.DurationSec = &duration
				case "progressing":
					rec.Progress = uint8(45 + s.rnd.Intn(30))
					rec.StartedAt = &startedAt
				case "failed":
					finished := startedAt.Add(time.Duration(duration) * time.Second)
					rec.Progress = 60
					rec.StartedAt = &startedAt
					rec.FinishedAt = &finished
					rec.DurationSec = &duration
					rec.ErrorCode = "READINESS_TIMEOUT"
					rec.AIDiagnosis = model.JSONText(
						`{"reason":"新版本启动时未能连上配置中心，Pod 处于 CrashLoopBackOff；` +
							`对比上一版本发现新增了对 CONFIG_CENTER_ADDR 的强依赖，但该环境变量未在部署参数中提供","suggestion":"在环境变量中补 CONFIG_CENTER_ADDR，或回滚到上一版本后先在测试区验证该依赖","confidence":0.89}`)
				case "pending_approval":
					rec.Progress = 0
					// 正式区需要审批：审批人留空，等真实审批动作回填
				case "queued":
					rec.Progress = 0
				}

				if err := tx.Create(&rec).Error; err != nil {
					return err
				}
				if err := tx.Create(deploySteps(rec.ID, status, startedAt, duration)).Error; err != nil {
					return err
				}

				// 回填服务的最近部署单
				if isLast {
					if err := tx.Model(&model.ServiceInstance{}).
						Where("id = ?", svcID).
						Update("last_deploy_id", rec.ID).Error; err != nil {
						return err
					}
				}
			}

			// 参数版本历史：每个服务留一版，记录当前生效的规格
			p := model.DeployParam{
				ServiceID:  svcID,
				EnvKey:     es.Key,
				ProjectKey: sp.Key,
				Version:    1,
				CPURequest: cpuRequestOf(sp.Type),
				CPULimit:   cpuLimitOf(sp.Type),
				MemRequest: memRequestOf(sp.Type),
				MemLimit:   memLimitOf(sp.Type),
				EnvVars:    model.JSONText(serviceEnvVars(es.Key, sp)),
				ConfigMaps: model.JSONText(fmt.Sprintf(`["%s-config"]`, sp.Key)),
				Secrets:    model.JSONText(fmt.Sprintf(`["%s-secret"]`, sp.Key)),
				Diff:       model.JSONText(`[]`),
				ChangedAt:  s.now.AddDate(0, 0, -(ei*2 + pi%5 + 1)),
				Note:       "初始参数",
			}
			if err := tx.Create(&p).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// rollbacks 生成回滚记录。回滚复用部署状态机，这里只补回滚语义字段。
func (s *seeder) rollbacks(tx *gorm.DB) error {
	s.init()

	targets := []struct {
		EnvKey, ProjectKey, Reason, TriggerType string
		AIConfidence                            *float64
	}{
		{"prod", "payment-service", "线上支付回调超时率突增，回滚到上一稳定版本", "manual", nil},
		{"prod", "order-service", "订单创建接口 5xx 比例上升，AI 建议回滚并已人工确认", "ai_suggested", ptrFloat(0.92)},
		{"staging", "report-service", "报表导出文件名乱码，回滚验证是否为本次变更引入", "manual", nil},
		{"canary", "user-service", "灰度期间登录成功率下降 0.4%，快速回滚", "manual", nil},
	}

	for i, t := range targets {
		svcID, ok := s.serviceIDs[t.EnvKey+"/"+t.ProjectKey]
		if !ok {
			continue
		}
		imgID, hasImg := s.imageIDs[t.ProjectKey]
		if !hasImg {
			continue
		}

		var dep model.DeployRecord
		if err := tx.Where("service_id = ?", svcID).
			Order("created_at DESC").First(&dep).Error; err != nil {
			continue
		}

		ageHours := 6 + s.rnd.Intn(200)
		at := s.now.Add(-time.Duration(ageHours) * time.Hour)
		duration := uint32(35 + s.rnd.Intn(120))
		operator := nameOfKey(t.ProjectKey)
		operatorID := s.userIDs[operator]
		if operatorID == 0 {
			operatorID = s.ownerIDs[0]
		}

		rec := model.RollbackRecord{
			DeployID:     dep.ID,
			DeployNo:     dep.DeployNo,
			EnvID:        s.envIDs[t.EnvKey],
			EnvKey:       t.EnvKey,
			ProjectID:    dep.ProjectID,
			ProjectKey:   t.ProjectKey,
			ServiceID:    svcID,
			FromImageID:  imgID,
			FromImage:    s.imageRefs[t.ProjectKey],
			ToImageID:    imgID,
			ToImage:      fmt.Sprintf("%s/%s/%s:previous", seedRegistry, projectTypeOf(t.ProjectKey), t.ProjectKey),
			Reason:       t.Reason,
			TriggerType:  t.TriggerType,
			AIConfidence: t.AIConfidence,
			OperatorID:   operatorID,
			OperatorName: operator,
			Status:       "success",
			DurationSec:  &duration,
			CreatedAt:    at,
			UpdatedAt:    at.Add(time.Duration(duration) * time.Second),
		}
		// 回滚前的镜像取上一条成功构建的镜像位置，这里用同仓库的 tag 递减表达
		if i%2 == 1 {
			rec.Status = "failed"
		}
		if err := tx.Create(&rec).Error; err != nil {
			return err
		}
	}
	return nil
}

func projectTypeOf(key string) string {
	for _, sp := range projectSpecs {
		if sp.Key == key {
			return sp.Type
		}
	}
	return "backend"
}

func ptrFloat(v float64) *float64 { return &v }
