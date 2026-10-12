package store

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/model"
)

// --------------------------------------------------------------------------- 环境

func (s *seeder) environments(tx *gorm.DB) error {
	s.init()

	for i, es := range envSpecs {
		lastSync := s.now.Add(-time.Duration(1+i*3) * time.Minute)
		approverIDs := s.approverIDsOf(es)
		env := model.Environment{
			EnvKey:        es.Key,
			Name:          es.Name,
			Cluster:       es.Cluster,
			ClusterVer:    es.ClusterVer,
			Namespace:     es.Namespace,
			DeployMethod:  es.DeployMethod,
			Registry:      seedRegistry,
			IngressDomain: es.Domain,
			NeedApproval:  es.Approval,
			ApproverIDs:   model.JSONText(approverIDs),
			OwnerID:       s.userIDOf(es.Owner),
			Description:   es.Description,
			Enabled:       es.Enabled,
			HealthStatus:  es.Health,
			CPUUsage:      es.CPU,
			MemUsage:      es.Mem,
			NodeCount:     es.Nodes,
			LastSyncAt:    &lastSync,
			CreatedAt:     s.now.AddDate(0, 0, -(2000 + i*37)),
			UpdatedAt:     s.now.Add(-time.Duration(2+i*11) * time.Hour),
		}
		if err := tx.Create(&env).Error; err != nil {
			return err
		}
		s.envIDs[es.Key] = env.ID

		// 环境级变量：与前端 ENV_SPECS 的 vars 一一对应
		vars := make([]model.EnvironmentVar, 0, len(es.Vars))
		for _, v := range es.Vars {
			vars = append(vars, model.EnvironmentVar{
				EnvID:       env.ID,
				VarKey:      v[0],
				VarValue:    v[1],
				IsSecret:    v[2] == "secret",
				Description: envVarDesc(v[0]),
			})
		}
		if err := tx.Create(&vars).Error; err != nil {
			return err
		}

		// 关联项目：直接用规格里枚举的清单，不做规则推导
		links := make([]model.EnvironmentProject, 0, len(es.ProjectKeys))
		for _, pk := range es.ProjectKeys {
			pid, ok := s.projectIDs[pk]
			if !ok {
				return fmt.Errorf("环境 %s 引用了不存在的项目 %s", es.Key, pk)
			}
			links = append(links, model.EnvironmentProject{EnvID: env.ID, ProjectID: pid})
		}
		if err := tx.Create(&links).Error; err != nil {
			return err
		}
	}
	return nil
}

// userIDOf 按姓名取用户 ID，取不到时退回第一个负责人，保证外键（逻辑上的）不为 0。
func (s *seeder) userIDOf(name string) uint64 {
	if id, ok := s.userIDs[name]; ok && id != 0 {
		return id
	}
	if len(s.ownerIDs) > 0 {
		return s.ownerIDs[0]
	}
	return 1
}

// approverIDsOf 生成审批人 ID 列表（JSON 数组字面量）。
func (s *seeder) approverIDsOf(es envSpec) string {
	if len(es.Approvers) == 0 {
		return "[]"
	}
	ids := make([]string, 0, len(es.Approvers))
	for _, name := range es.Approvers {
		ids = append(ids, fmt.Sprint(s.userIDOf(name)))
	}
	return "[" + strings.Join(ids, ",") + "]"
}

// envVarDesc 给常见环境变量补一句说明，让「环境变量」页更可读。
func envVarDesc(key string) string {
	switch key {
	case "SPRING_PROFILES_ACTIVE":
		return "Spring 生效的 profile"
	case "LOG_LEVEL":
		return "服务日志级别"
	case "DB_HOST":
		return "数据库地址"
	case "DB_PASSWORD":
		return "数据库密码"
	case "REDIS_ADDR":
		return "缓存地址"
	case "TRACE_SAMPLE_RATE":
		return "链路采样率"
	case "SENTRY_DSN":
		return "前端异常上报地址"
	case "HPA_ENABLED":
		return "是否启用自动扩缩容"
	case "GRAY_WEIGHT":
		return "灰度流量权重（%）"
	case "MOCK_ENABLED":
		return "是否启用接口 Mock"
	}
	return ""
}

// projectKeysInEnv 返回某环境纳管的项目 key 列表。
func projectKeysInEnv(envKey string) []string {
	for _, es := range envSpecs {
		if es.Key == envKey {
			return es.ProjectKeys
		}
	}
	return nil
}

func logLevelOf(key string) string {
	if key == "test" || key == "perf" {
		return "DEBUG"
	}
	if key == "prod" {
		return "WARN"
	}
	return "INFO"
}

// --------------------------------------------------------------------------- 构建与镜像

func (s *seeder) buildsAndImages(tx *gorm.DB) error {
	s.init()

	for pi, sp := range projectSpecs {
		pid := s.projectIDs[sp.Key]
		total := 5 + s.rnd.Intn(5) // 每个项目 5~9 条历史构建
		imgName := fmt.Sprintf("%s/%s", sp.Type, sp.Key)

		for bi := 0; bi < total; bi++ {
			buildNo := uint32(bi + 1)
			// 越靠后的构建时间越近：最早约 30 天前，最新在 2 小时内
			ageMin := (total - bi) * (240 + s.rnd.Intn(900))
			startedAt := s.now.Add(-time.Duration(ageMin) * time.Minute)
			duration := uint32(80 + s.rnd.Intn(280))
			branch, commit, msg := s.commitOf(sp, bi)
			isLast := bi == total-1

			status := s.buildStatus(sp, pi, isLast)
			tag := fmt.Sprintf("%s-%s-%s", branch, commit, startedAt.Format("20060102150405"))
			image := fmt.Sprintf("%s/%s:%s", seedRegistry, imgName, tag)

			// IdempotencyKey 是"可空唯一列"，Go 侧必须用指针表达：
			// 空串在 MySQL 唯一索引里会互相冲突，只有 NULL 才允许重复。
			idem := fmt.Sprintf("seed-%s-%d", sp.Key, buildNo)
			rec := model.BuildRecord{
				BuildNo:        buildNo,
				ProjectID:      pid,
				ProjectKey:     sp.Key,
				Branch:         branch,
				CommitID:       commit,
				CommitMsg:      msg,
				TriggerType:    triggerTypeOf(sp.Key, bi),
				TriggerName:    triggerNameOf(sp.Key, bi),
				Status:         status,
				DurationSec:    &duration,
				IdempotencyKey: &idem,
				CreatedAt:      startedAt,
				UpdatedAt:      startedAt,
			}

			switch status {
			case "running":
				rec.Stage = "推送镜像"
				rec.Progress = uint8(35 + s.rnd.Intn(50))
				rec.StartedAt = &startedAt
				rec.DurationSec = nil
			case "queued":
				rec.Stage = "等待 Runner"
				rec.Progress = 0
				rec.DurationSec = nil
			case "success":
				finished := startedAt.Add(time.Duration(duration) * time.Second)
				rec.Stage = "完成"
				rec.Progress = 100
				rec.StartedAt = &startedAt
				rec.FinishedAt = &finished
				rec.LogSize = uint32(2048 + s.rnd.Intn(40000))
			case "failed":
				finished := startedAt.Add(time.Duration(duration) * time.Second)
				rec.Stage = "构建失败"
				rec.Progress = uint8(45 + s.rnd.Intn(40))
				rec.StartedAt = &startedAt
				rec.FinishedAt = &finished
				rec.ErrorCode = failureCodeOf(sp)
				rec.AIDiagnosis = model.JSONText(aiDiagnosisOf(sp))
				rec.LogSize = uint32(2048 + s.rnd.Intn(20000))
			}

			if err := tx.Create(&rec).Error; err != nil {
				return err
			}

			// 成功构建产出镜像；只为最近三次成功构建留记录，避免数据过量
			if status == "success" && bi >= total-3 {
				pushedAt := startedAt
				if rec.FinishedAt != nil {
					pushedAt = *rec.FinishedAt
				}
				img := model.ImageRecord{
					ProjectID:   pid,
					ProjectKey:  sp.Key,
					Registry:    seedRegistry,
					Repo:        imgName,
					Tag:         tag,
					Image:       image,
					Digest:      "sha256:" + s.randHex(32),
					SizeBytes:   ptrUint64(s.imageSizeOf(sp)),
					Arch:        "amd64",
					Branch:      branch,
					CommitID:    commit,
					BuildID:     rec.ID,
					BuildNo:     buildNo,
					PushedAt:    pushedAt,
					PullCount:   uint32(s.rnd.Intn(400)),
					DeployedEnv: model.JSONText(`[]`),
					CreatedAt:   startedAt,
				}
				if err := tx.Create(&img).Error; err != nil {
					return err
				}
				s.imageIDs[sp.Key] = img.ID
				s.imageRefs[sp.Key] = image
				s.latestTag[sp.Key] = tag

				// 回填构建记录关联的镜像
				if err := tx.Model(&model.BuildRecord{}).Where("id = ?", rec.ID).
					Update("image_record_id", img.ID).Error; err != nil {
					return err
				}
			}

			// 只为最近两条构建写日志明细（更早的视为已归档），控制数据体量
			if bi >= total-2 {
				if err := s.writeBuildLogs(tx, rec.ID, sp, status, startedAt); err != nil {
					return err
				}
			}
		}

		s.buildNoSeen[sp.Key] = uint32(total)
	}
	return nil
}

// writeBuildLogs 生成一次构建的日志行。行内容与真实 CI 输出保持同构，
// 这样前端日志面板（含行首时间戳）接入后不需要改渲染逻辑。
func (s *seeder) writeBuildLogs(tx *gorm.DB, buildID uint64, sp projectSpec, status string, startedAt time.Time) error {
	lines := buildLogLines(sp)
	logs := make([]model.BuildLog, 0, len(lines))
	at := startedAt
	for i, ln := range lines {
		at = at.Add(time.Duration(300+s.rnd.Intn(2500)) * time.Millisecond)
		level := "info"
		if strings.HasPrefix(ln, "$ ") {
			level = "cmd"
		} else if strings.Contains(ln, "ERROR") || strings.Contains(ln, "error:") {
			level = "error"
		} else if strings.Contains(ln, "WARN") {
			level = "warn"
		}
		logs = append(logs, model.BuildLog{
			BuildID:  buildID,
			Seq:      uint32(i + 1),
			Level:    level,
			Content:  trimLog(ln),
			LoggedAt: at,
		})
	}
	if status == "running" {
		// 未完成的构建只保留已产出的部分日志
		keep := len(logs) * 2 / 3
		if keep < 1 {
			keep = 1
		}
		logs = logs[:keep]
	}
	return tx.Create(&logs).Error
}

func buildLogLines(sp projectSpec) []string {
	tag := sampleTag(sp)
	head := []string{
		fmt.Sprintf("$ git clone --depth 1 -b master git@%s:%s/%s.git", seedGitLab, sp.Type, sp.Key),
		"Cloning into '" + sp.Key + "'...",
		"remote: Enumerating objects: 1284, done.",
		"Receiving objects: 100% (1284/1284), 2.41 MiB | 8.20 MiB/s, done.",
		fmt.Sprintf("$ cd %s && docker build -f Dockerfile -t %s/%s/%s:%s .", sp.Key, seedRegistry, sp.Type, sp.Key, tag),
	}
	var mid []string
	if sp.Type == "frontend" {
		mid = []string{
			"#5 [deps 1/3] COPY package*.json ./",
			"#6 npm ci --registry=https://registry.npmmirror.com",
			"added 1284 packages in 42s",
			"#9 [build 1/2] RUN npm run build",
			"vite v5.4.2 building for production...",
			"✓ 1284 modules transformed.",
			"dist/assets/index-8fJ2k.js   412.38 kB │ gzip: 128.44 kB",
			"✓ built in 18.42s",
		}
	} else {
		mid = []string{
			"#5 [builder 2/6] COPY pom.xml .",
			"#6 [builder 3/6] RUN mvn -B -DskipTests -U dependency:go-offline",
			"[INFO] BUILD SUCCESS",
			"#9 [builder 6/6] RUN mvn -B -DskipTests package",
			"[INFO] Building jar: /build/target/app.jar",
			"[INFO] Total time:  01:12 min",
		}
	}
	tail := []string{
		fmt.Sprintf("#12 pushing %s/%s/%s:%s", seedRegistry, sp.Type, sp.Key, tag),
		"#12 pushing layers",
		"#12 pushing manifest",
		"#12 DONE 3.2s",
		"构建成功，镜像已推送",
	}
	out := append(append(head, mid...), tail...)
	return out
}

// sampleTag 生成示例镜像 tag，仅用于种子日志展示（与真实构建无关）。
//
// 用项目标识做确定性派生而不是随机数：种子数据的目的是让列表页看起来可信，
// 同一个项目每次初始化都该得到同样的 tag，否则"重新灌一次种子数据"
// 会让截图与文档对不上。
func sampleTag(sp projectSpec) string {
	h := 0
	for _, r := range sp.Key {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return fmt.Sprintf("master-%06x", h%0xffffff)
}

func trimLog(s string) string {
	if len(s) > 2000 {
		return s[:2000]
	}
	return s
}

// buildStatus 决定一条构建记录的状态。
// 最新一条按项目序号错落分布 running / failed，其余按项目期望成功率生成。
func (s *seeder) buildStatus(sp projectSpec, pi int, isLast bool) string {
	if isLast {
		switch {
		case pi%7 == 3:
			return "running"
		case pi%11 == 5:
			return "failed"
		case pi == 19: // 已停用的项目，最新一次是取消
			return "canceled"
		}
		return "success"
	}
	if s.rnd.Intn(100) < sp.Rate {
		return "success"
	}
	return "failed"
}

func failureCodeOf(sp projectSpec) string {
	if sp.Type == "frontend" {
		return "NPM_INSTALL"
	}
	return "COMPILE_ERROR"
}

func aiDiagnosisOf(sp projectSpec) string {
	if sp.Type == "frontend" {
		return `{"reason":"依赖安装失败：npmmirror 源上缺少 @yunnian/shared@2.6.1 版本，npm ci 因 lock 文件与 registry 不一致而中断","suggestion":"确认 @yunnian/shared 已发版并同步至私服；若为临时依赖，可先在 package.json 中锁定可用版本，或改用 npm install 后重建 lock 文件","confidence":0.86}`
	}
	return `{"reason":"编译失败：OrderController 引用了 com.yunnian.common.dto.PageQuery，但依赖 yunnian-common 仍是 2.5.x，该类型在 2.6.0 才引入","suggestion":"将 pom.xml 中 yunnian-common 版本升至 2.6.0 及以上；建议同时检查是否遗漏了 release 分支的 cherry-pick","confidence":0.91}`
}

// imageSizeOf 生成确定性的镜像体积。用 seeder 的随机源以保证可复现。
func (s *seeder) imageSizeOf(sp projectSpec) uint64 {
	if sp.Type == "frontend" {
		return uint64(18+s.rnd.Intn(30)) << 20
	}
	return uint64(76+s.rnd.Intn(140)) << 20
}

// projectsInEnv 决定某项目是否部署在某环境。
//
// 规则刻意贴近真实团队的做法，而不是"全部环境全量部署"：
//   - 测试区：全量（联调需要）
//   - 性能区：只压后端与核心前端（压测成本高）
//   - 预发：大部分，但排除一部分（不是所有项目都有预发需求）
//   - 灰度：只放少数核心链路
//   - 正式：排除灰度验证用的与最后一个（已下线）
//
// 这样前端「发布拓扑」页在不同环境下看到的服务数量差异是"有理由的"，
// 而不是随机生成的噪声。
func projectsInEnv(envKey string, idx int, typ string) bool {
	switch envKey {
	case "test":
		return true
	case "perf":
		return typ == "backend" || idx < 4
	case "staging":
		return idx%3 != 2
	case "canary":
		return idx%5 == 4
	case "prod":
		return idx%5 != 3 && idx != len(projectSpecs)-1
	}
	return false
}

func triggerTypeOf(key string, bi int) string {
	if key == "user-service" || key == "gateway-service" {
		if bi%3 == 0 {
			return "webhook"
		}
	}
	if bi%5 == 0 {
		return "schedule"
	}
	return "manual"
}

func triggerNameOf(key string, bi int) string {
	if triggerTypeOf(key, bi) == "webhook" {
		return "GitLab Webhook"
	}
	if triggerTypeOf(key, bi) == "schedule" {
		return "定时任务"
	}
	return nameOfKey(key)
}

func nameOfKey(key string) string {
	for _, sp := range projectSpecs {
		if sp.Key == key {
			return sp.Owner
		}
	}
	return "系统管理员"
}

// commitOf 生成项目第 bi 次构建对应的分支与提交信息。
func (s *seeder) commitOf(sp projectSpec, bi int) (branch, commit, msg string) {
	branches := []string{defaultBranchOf(sp.Key), "release/2.6", "feature/pay-refactor", "hotfix/login"}
	branch = branches[bi%len(branches)]
	if bi%7 == 2 {
		branch = "release/2.6"
	}
	msgs := []string{
		"feat(订单): 支持部分退款场景",
		"fix(用户): 修复登录态续期偶发失效",
		"refactor(商品): 收敛库存扣减逻辑",
		"chore: 升级基础镜像到 alpine 3.20",
		"feat(网关): 新增按租户限流规则",
		"fix(支付): 修正对账金额精度问题",
		"perf(搜索): 减少 ES 查询次数",
		"docs: 补充部署说明",
	}
	return branch, s.randHex(3) + s.randHex(1), msgs[(bi+len(sp.Key))%len(msgs)]
}

func (s *seeder) randHex(n int) string {
	const digits = "0123456789abcdef"
	buf := make([]byte, n*2)
	for i := range buf {
		buf[i] = digits[s.rnd.Intn(len(digits))]
	}
	return string(buf)
}

// --------------------------------------------------------------------------- 服务实例

// forcedServiceStatus 复刻前端原型里刻意造成的异常态，
// 保证前端「发布拓扑」页在切换到真实接口后，视觉呈现与原型一致。
var forcedServiceStatus = map[string]string{
	"test/order-service":     "deploying",
	"perf/report-service":    "error",
	"staging/payment-service": "deploying",
	"test/search-service":    "pending",
	"prod/user-service":      "pending",
}

func (s *seeder) services(tx *gorm.DB) error {
	s.init()
	updater := s.userIDs["系统管理员"]

	for ei, es := range envSpecs {
		envID := s.envIDs[es.Key]
		replicas := targetReplicas(es.Key)

		for pi, sp := range projectSpecs {
			if !projectsInEnv(es.Key, pi, sp.Type) {
				continue
			}
			envKeyProj := es.Key + "/" + sp.Key
			status := "running"
			if v, ok := forcedServiceStatus[envKeyProj]; ok {
				status = v
			}

			tag := s.latestTag[sp.Key]
			image := s.imageRefs[sp.Key]
			if tag == "" {
				// 该环境部署的项目还没有成功构建产出镜像时，给一个占位版本
				tag = "master-0000000-20260101000000"
				image = fmt.Sprintf("%s/%s/%s:%s", seedRegistry, sp.Type, sp.Key, tag)
			}

			ready := replicas
			switch status {
			case "deploying":
				ready = replicas - 1
			case "error":
				ready = 0
			case "pending":
				ready = 0
			case "stopped":
				ready = 0
			}

			updatedAt := s.now.Add(-time.Duration(1+ei*7+pi) * time.Hour)
			svc := model.ServiceInstance{
				EnvID:         envID,
				ProjectID:     s.projectIDs[sp.Key],
				EnvKey:        es.Key,
				ProjectKey:    sp.Key,
				WorkloadKind:  workloadKindOf(sp.Key),
				WorkloadName:  sp.Key,
				CurrentImgID:  ptrUint64OrNil(s.imageIDs[sp.Key]),
				CurrentImage:  image,
				CurrentTag:    tag,
				Replicas:      replicas,
				ReadyReplicas: ready,
				Status:        status,
				CPURequest:    cpuRequestOf(sp.Type),
				CPULimit:      cpuLimitOf(sp.Type),
				MemRequest:    memRequestOf(sp.Type),
				MemLimit:      memLimitOf(sp.Type),
				EnvVars:       model.JSONText(serviceEnvVars(es.Key, sp)),
				ConfigMaps:    model.JSONText(fmt.Sprintf(`["%s-config"]`, sp.Key)),
				Secrets:       model.JSONText(fmt.Sprintf(`["%s-secret"]`, sp.Key)),
				Entrypoint:    fmt.Sprintf("https://%s.%s", sp.Key, strings.TrimPrefix(es.Domain, "*.")),
				LivenessOK:    status != "error",
				ReadinessOK:   status == "running",
				UpdatedAt:     updatedAt,
			}
			if updater != 0 {
				svc.UpdatedBy = &updater
			}
			if err := tx.Create(&svc).Error; err != nil {
				return err
			}
			s.serviceIDs[envKeyProj] = svc.ID
		}
	}
	return nil
}

func targetReplicas(envKey string) int {
	switch envKey {
	case "test":
		return 1
	case "perf", "staging", "canary":
		return 2
	case "prod":
		return 3
	}
	return 1
}

func workloadKindOf(key string) string {
	// 有状态组件用 StatefulSet；平台里目前只有报表与搜索属于这一类
	if key == "report-service" || key == "search-service" {
		return "StatefulSet"
	}
	return "Deployment"
}

func serviceEnvVars(envKey string, sp projectSpec) string {
	return fmt.Sprintf(
		`[{"key":"SPRING_PROFILES_ACTIVE","value":"%s"},`+
			`{"key":"LOG_LEVEL","value":"%s"},`+
			`{"key":"APP_NAME","value":"%s"},`+
			`{"key":"DB_PASSWORD","value":"••••••••","secret":true}]`,
		envKey, logLevelOf(envKey), sp.Key)
}

func cpuRequestOf(typ string) string {
	if typ == "frontend" {
		return "100m"
	}
	return "200m"
}

func cpuLimitOf(typ string) string {
	if typ == "frontend" {
		return "500m"
	}
	return "1000m"
}

func memRequestOf(typ string) string {
	if typ == "frontend" {
		return "128Mi"
	}
	return "512Mi"
}

func memLimitOf(typ string) string {
	if typ == "frontend" {
		return "512Mi"
	}
	return "2Gi"
}

func ptrUint64(v uint64) *uint64 { return &v }

func ptrUint64OrNil(v uint64) *uint64 {
	if v == 0 {
		return nil
	}
	return &v
}

func ptrTime(t time.Time) *time.Time { return &t }
