package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yunnian/yunnian/internal/crypto"
	"github.com/yunnian/yunnian/internal/errs"
	"github.com/yunnian/yunnian/internal/integration"
	"github.com/yunnian/yunnian/internal/model"
)

// --------------------------------------------------------------------------- K8s 集群接入
//
// 本文件把「集群」从 environment.cluster 里的一段自由文本变成**有凭据、有真实探测结果的实体**。
// 三条不可动摇的约束，改动前请先读：
//
//  1. **kubeconfig 只进不出**。它含 client-key / token，属集群管理员级凭据。
//     任何 DTO 都不得携带明文或密文，只回 `hasKubeconfig` 布尔。
//     因此编辑时 kubeconfig 留空表示"不修改"，而不是"清空"。
//
//  2. **落库前先探测，探测不过就不落库**。这是本功能唯一的存在理由——
//     原先 LocalCluster 的 Ping 恒返回 nil，「测试连接」永远全绿，
//     填一个拼错的集群名也绿。现在"接入成功"由集群自己回答。
//
//  3. **探测的是将要保存的那份 kubeconfig**（最小化后的单 context），不是用户粘贴的原文。
//     顺序反过来会引入一个很难发现的漏洞：原文里 current-context 指向集群 A 时探测通过，
//     而我们要存的 context 指向集群 B（不可达），于是"接入成功"的结论与保存的凭据无关。

// clusterCredScope secret_credential 的 scope。一个集群一条凭据，名字就是集群标识。
const clusterCredScope = "k8s_cluster"

// clusterKeyRe 集群标识规则。与前端 ClusterAccess.tsx 的 KEY_RE 保持一致：
// 标识会被写进 environment.cluster、会被当文件名/工单号引用，因此限制在小写字母数字与 . _ -。
var clusterKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// normalizeClusterRef 归一化「环境里填的集群标识」，用于判断引用关系。
//
// 必须与前端 config/topology.ts 的 clusterKeyOf 同口径：同一个集群在存量数据里
// 有两个名字（种子里 canary 是 `k8s-prod-01`、prod 是 `k8s-prod-01（双可用区）`）。
// 若这里不去括号，删集群时会漏掉那些带后缀的引用者——用户以为没人用，
// 删掉之后一批环境的集群指向空。**引用检查宁可多拦，不可漏拦。**
func normalizeClusterRef(cluster string) string {
	c := strings.TrimSpace(cluster)
	if i := strings.IndexAny(c, "（("); i >= 0 {
		c = c[:i]
	}
	return strings.TrimSpace(c)
}

// clusterProbeTimeout 探测超时。复用 integration.kubernetes.timeout，
// 因为它表达的就是"与集群通信等多久"，与部署路径用的是同一个语义。
func (s *SystemService) clusterProbeTimeout() time.Duration {
	t := s.deps.Cfg.Integration.Kubernetes.Timeout.D()
	if t <= 0 {
		t = 15 * time.Second
	}
	return t
}

// --------------------------------------------------------------------------- 读

// ListClusters 集群列表（只读，不含任何凭据内容）。
//
// 顺带算出 usedByEnvs：删除按钮要靠它预先禁用。放在这里而不是让前端逐个去问，
// 是因为列表页一次要画 N 行，逐个问就是 N 次请求。
func (s *SystemService) ListClusters(ctx context.Context) ([]K8sClusterDTO, error) {
	var rows []model.K8sCluster
	if err := s.db(ctx).Order("cluster_key ASC").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询集群列表失败")
	}
	used, err := s.clusterUsedBy(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]K8sClusterDTO, 0, len(rows))
	for i := range rows {
		out = append(out, clusterToDTO(&rows[i], used[rows[i].Key]))
	}
	return out, nil
}

// clusterUsedBy 环境引用关系：集群标识 → 引用它的环境 key 列表。
func (s *SystemService) clusterUsedBy(ctx context.Context) (map[string][]string, error) {
	var rows []model.Environment
	if err := s.db(ctx).Select("env_key", "cluster").Where("cluster <> ''").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询环境引用的集群失败")
	}
	out := make(map[string][]string, len(rows))
	for _, r := range rows {
		k := normalizeClusterRef(r.Cluster)
		if k == "" {
			continue
		}
		out[k] = append(out[k], r.EnvKey)
	}
	return out, nil
}

// --------------------------------------------------------------------------- 解析

// ParseClusterKubeconfig 解析一份 kubeconfig，列出可接入的 context。
//
// 不落库、不建连接：这一步只是把文件摊开给用户挑。"能不能连"要到真正接入时
// 才会被回答（见 CreateClusters），这样用户能先看清有哪些 context，
// 而不是粘完就要立刻承担一次网络尝试。
func (s *SystemService) ParseClusterKubeconfig(ctx context.Context, in ClusterParseInput) (*ClusterParseResultDTO, error) {
	parsed, err := integration.ParseKubeconfig(in.Kubeconfig)
	if err != nil {
		// **按契约返回 ok=false，而不是抛错。**
		//
		// 前端（api/cluster.ts 的 parse 注释）写明了"解析失败时 ok=false + errors，
		// 不抛异常"——早先这里 return errs.Invalid(...)，走的是 40000 + 一句 toast，
		// 结果前端那个**内联的错误面板永远走不到**（一个不会出现的分支 = 假承诺）。
		// 而且解析失败往往是"这份文件的若干处不对"，内联列出原因比一闪而过的
		// toast 有用得多。契约怎么写的就怎么做。
		return &ClusterParseResultDTO{
			OK:       false,
			Errors:   []string{err.Error()},
			Contexts: []ClusterContextDTO{},
		}, nil
	}

	// 已接入判定：按 (apiServer, context) 比对。同一个集群用不同 context 名接两次
	// 是允许的（比如换个 ServiceAccount），所以不能只按 apiServer 去重。
	var rows []model.K8sCluster
	if err := s.db(ctx).Select("api_server", "context_name").Find(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "查询已接入集群失败")
	}
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		seen[r.APIServer+"\x00"+r.ContextName] = struct{}{}
	}

	out := &ClusterParseResultDTO{
		OK:             true,
		// Errors 显式初始化成空切片：Go 的 nil 切片会序列化成 `null`，
		// 而前端把它声明成 `string[]`（见 api/cluster.ts 的列表字段收口说明）。
		Errors:         []string{},
		CurrentContext: parsed.CurrentContext,
		Contexts:       make([]ClusterContextDTO, 0, len(parsed.Contexts)),
	}
	for _, c := range parsed.Contexts {
		_, dup := seen[c.APIServer+"\x00"+c.Context]
		out.Contexts = append(out.Contexts, ClusterContextDTO{
			Context:         c.Context,
			Cluster:         c.Cluster,
			APIServer:       c.APIServer,
			Namespace:       c.Namespace,
			User:            c.User,
			CredentialKind:  c.CredentialKind,
			Problem:         c.Problem,
			AlreadyImported: dup,
		})
	}
	return out, nil
}

// --------------------------------------------------------------------------- 接入

// CreateClusters 接入一批 context。**部分成功语义**。
//
// 为什么不整批回滚：一次导入常含多个 context，其中某个集群网络不通是常事。
// 整批回滚会让用户挨个重试；只回成功项则用户不知道另外几个为什么没出现。
// 所以失败的必须带 context 名与真实原因单独回来。
func (s *SystemService) CreateClusters(ctx context.Context, in ClusterCreateInput, c Caller) (*ClusterCreateResultDTO, error) {
	if len(in.Clusters) == 0 {
		return nil, errs.Invalid("没有要接入的集群")
	}
	timeout := s.clusterProbeTimeout()
	used, err := s.clusterUsedBy(ctx)
	if err != nil {
		return nil, err
	}

	// Clusters / Errors 都显式初始化成空切片，理由同解析结果：nil 切片会变成 JSON null，
	// 而前端按数组读（`res.clusters?.length` / `fails.map(...)`）。
	out := &ClusterCreateResultDTO{Clusters: []K8sClusterDTO{}, Errors: []ClusterCreateError{}}
	var failures []ClusterCreateError
	// 同一次请求里出现重复标识要单独拦：否则第二条会撞唯一索引，
	// 报出来的是一句数据库错误，而不是"这两个 context 被你起了同一个名字"。
	pending := make(map[string]string, len(in.Clusters))

	for _, item := range in.Clusters {
		key := strings.TrimSpace(item.Key)
		label := stringOr(strings.TrimSpace(item.Context), key)
		if key == "" {
			failures = append(failures, ClusterCreateError{Context: label, Message: "集群标识不能为空"})
			continue
		}
		if !clusterKeyRe.MatchString(key) {
			failures = append(failures, ClusterCreateError{
				Context: label,
				Message: "集群标识只能用小写字母、数字与 . _ -，且需以字母或数字开头",
			})
			continue
		}
		if prev, dup := pending[key]; dup {
			failures = append(failures, ClusterCreateError{
				Context: label,
				Message: fmt.Sprintf("本次导入里 %q 与 %q 用了同一个集群标识，请改成不同的", prev, label),
			})
			continue
		}
		pending[key] = label
		if strings.TrimSpace(item.Kubeconfig) == "" {
			failures = append(failures, ClusterCreateError{Context: label, Message: "缺少 kubeconfig 内容"})
			continue
		}

		// 先最小化，再探测——顺序不能反，原因见本文件顶部约束 3。
		minimal, err := integration.MinimalKubeconfig(item.Kubeconfig, item.Context)
		if err != nil {
			failures = append(failures, ClusterCreateError{Context: label, Message: err.Error()})
			continue
		}
		probe := integration.Probe(ctx, []byte(minimal), timeout)
		if !probe.OK {
			failures = append(failures, ClusterCreateError{Context: label, Message: probe.Message})
			continue
		}

		apiServer, kubeCluster := integration.ContextInfoOf(item.Kubeconfig, item.Context)
		row, err := s.persistCluster(ctx, clusterPersistInput{
			Key:         key,
			Name:        stringOr(strings.TrimSpace(item.Name), key),
			Context:     item.Context,
			Minimal:     minimal,
			Remark:      item.Remark,
			Probe:       probe,
			APIServer:   apiServer,
			KubeCluster: kubeCluster,
			Caller:      c,
		})
		if err != nil {
			failures = append(failures, ClusterCreateError{Context: label, Message: errs.From(err).Message})
			continue
		}
		s.audit(ctx, c, auditInput{
			Module: "系统", Action: "cluster:create", TargetType: "k8s_cluster",
			TargetID: &row.ID, TargetName: row.Name,
			Detail: fmt.Sprintf("接入 K8s 集群 %s（%s，%s）", row.Key, row.APIServer, probe.K8sVersion),
		})
		out.Clusters = append(out.Clusters, clusterToDTO(row, used[row.Key]))
	}
	if len(failures) > 0 {
		out.Errors = failures
	}
	return out, nil
}

// clusterPersistInput 落库入参（接入与改凭据共用）。
//
// APIServer 与 KubeCluster 由调用方从 kubeconfig 解析后传入，而不是在这里再解析一次：
// 解析器已经在 MinimalKubeconfig 里跑过，再解析一遍会出现两份可能不一致的结果
// （例如调用方传的是原文、这里传的是最小化文本）。
type clusterPersistInput struct {
	Key         string
	Name        string
	Context     string
	Minimal     string
	Remark      string
	Probe       *integration.ClusterProbe
	APIServer   string
	KubeCluster string
	Caller      Caller
}

// persistCluster 加密凭据并写集群行。
//
// 一个事务：不能出现"集群行建了、凭据没存"这种半截状态——那会让集群列表显示
// 「已配置凭据」，一点探测却报"凭据不存在"，而用户没有任何办法自查。
func (s *SystemService) persistCluster(ctx context.Context, in clusterPersistInput) (*model.K8sCluster, error) {
	err := s.tx(ctx, func(tx *gorm.DB) error {
		// 已有**未软删**的同标识集群时先拒绝。
		// 这里不做 UPSERT 覆盖：用户点的是"接入"，不是"覆盖"，
		// 静默换掉一个正被环境引用的集群的凭据，后果是下次发布跑到别的集群上。
		exists, err := s.loadClusterByKey(tx, in.Key)
		if err != nil {
			return err
		}
		if exists != nil {
			return errs.Conflict(fmt.Sprintf(
				"集群标识 %s 已被占用：该标识已存在，请换一个标识，或先删除原集群再接入", in.Key))
		}

		credID, err := sealKubeconfigCredential(tx, s.deps.Sealer, clusterCredScope, in.Key, in.Minimal, in.Name, in.Caller.UserID())
		if err != nil {
			return err
		}

		row := model.K8sCluster{
			Key:               in.Key,
			Name:              in.Name,
			APIServer:         in.APIServer,
			ContextName:       in.Context,
			KubeconfigCluster: in.KubeCluster,
			CredID:            credID,
			Remark:            in.Remark,
			CreatedBy:         in.Caller.UserID(),
			UpdatedBy:         in.Caller.UserID(),
		}
		applyProbe(&row, in.Probe)

		// 软删行会一直占着 uk_cluster_key（该索引不含 deleted_at），
		// 于是"删掉再想用同一个标识重建"会撞唯一键并报 40900——
		// project 表已经踩过这个坑两次（见 README 已知项）。
		// 这里直接捞回软删行复活，而不是把同一个缺陷再复制一遍。
		var trashed model.K8sCluster
		findErr := tx.Unscoped().Where("cluster_key = ?", in.Key).First(&trashed).Error
		switch {
		case findErr == nil:
			// 复活：deleted_at 置空，其余列全部按本次接入的结果重写。
			// 注意 created_at 保留原值——它记录的是"这个标识第一次被接入"的时间，
			// 抹掉它会让审计链断在这里。
			if err := tx.Unscoped().Model(&model.K8sCluster{}).Where("id = ?", trashed.ID).
				Updates(map[string]any{
					"name":                     row.Name,
					"api_server":               row.APIServer,
					"context_name":             row.ContextName,
					"kubeconfig_cluster":       row.KubeconfigCluster,
					"kubeconfig_credential_id": row.CredID,
					"status":                   row.Status,
					"k8s_version":              row.K8sVersion,
					"nodes":                    row.Nodes,
					"nodes_ready":              row.NodesReady,
					"namespace_count":          row.NamespaceCount,
					"last_probe_message":       row.LastProbeMessage,
					"last_probe_ms":            row.LastProbeMS,
					"last_probed_at":           row.LastProbedAt,
					"remark":                   row.Remark,
					"updated_by":               row.UpdatedBy,
					"updated_at":               now(),
					"deleted_at":               nil,
				}).Error; err != nil {
				return wrapDB(err, "恢复集群记录")
			}
			return nil
		case errors.Is(findErr, gorm.ErrRecordNotFound):
			if err := tx.Create(&row).Error; err != nil {
				return wrapDB(err, "创建集群记录")
			}
			return nil
		default:
			return wrapDB(findErr, "查询集群记录")
		}
	})
	if err != nil {
		return nil, err
	}
	// 重新读一次拿最终状态：复活路径走的是 Updates，内存里的 row 没有 ID，
	// 而接口响应需要完整的行（含 createdAt）。
	return s.clusterByKey(ctx, in.Key)
}

// applyProbe 把探测结果写进行对象。
// **不碰 APIServer**：那个地址来自 kubeconfig 解析，不是探测的产物
// （探测只回答"通不通、版本多少、几个节点"）。
func applyProbe(row *model.K8sCluster, p *integration.ClusterProbe) {
	if p == nil {
		row.Status = model.ClusterStatusUnprobed
		return
	}
	row.Status = p.Status
	row.K8sVersion = p.K8sVersion
	row.Nodes, row.NodesReady, row.NamespaceCount = p.Nodes, p.NodesReady, p.NamespaceCount
	row.LastProbeMessage = probeMessage(p)
	ms := p.LatencyMS
	row.LastProbeMS = &ms
	at := now()
	row.LastProbedAt = &at
}

// envCredScope 环境自带 kubeconfig 的 scope（环境表单里的「独立 kubeconfig」）。
const envCredScope = "environment"

// kubeCredDescription 凭据的管理员可读说明：在库里翻 secret_credential 时
// 一眼能看出这条属于集群还是环境，而不是满屏一样的 "kubeconfig"。
func kubeCredDescription(scope, key, name string) string {
	if scope == envCredScope {
		return fmt.Sprintf("环境 %s 自带的 kubeconfig（%s）", key, name)
	}
	return fmt.Sprintf("K8s 集群 %s 的 kubeconfig（%s）", key, name)
}

// sealKubeconfigCredential 加密并 upsert 一条 kubeconfig 凭据，返回其 ID。
//
// scope 区分归属（集群 k8s_cluster / 环境 environment）：同一个标识在两种 scope 下
// 各存各的，不会互相覆盖。plain 必须是**已最小化**的 kubeconfig ——
// 只保留要用到的那个 context，否则一份"只该访问测试集群"的凭据会连带握着生产集群的 client-key。
func sealKubeconfigCredential(tx *gorm.DB, sealer *crypto.Sealer, scope, key, plain, name string, userID *uint64) (*uint64, error) {
	sealed, err := sealer.Seal(plain)
	if err != nil {
		return nil, errs.Internalf(err, "加密 kubeconfig 失败")
	}
	var cred model.SecretCredential
	findErr := tx.Where("scope = ? AND name = ?", scope, key).First(&cred).Error
	switch {
	case findErr == nil:
		if err := tx.Model(&model.SecretCredential{}).Where("id = ?", cred.ID).Updates(map[string]any{
			"type":       "kubeconfig",
			"cipher":     sealed.Cipher,
			"iv":         sealed.IV,
			"auth_tag":   sealed.AuthTag,
			"updated_at": now(),
		}).Error; err != nil {
			return nil, wrapDB(err, "更新集群凭据")
		}
		return &cred.ID, nil
	case errors.Is(findErr, gorm.ErrRecordNotFound):
		cred = model.SecretCredential{
			Name:        key,
			Type:        "kubeconfig",
			Scope:       scope,
			Cipher:      sealed.Cipher,
			IV:          sealed.IV,
			AuthTag:     sealed.AuthTag,
			KeyVersion:  1,
			Description: kubeCredDescription(scope, key, name),
			CreatedBy:   userID,
		}
		if err := tx.Create(&cred).Error; err != nil {
			return nil, wrapDB(err, "保存集群凭据")
		}
		return &cred.ID, nil
	default:
		return nil, wrapDB(findErr, "查询集群凭据")
	}
}

// --------------------------------------------------------------------------- 改 / 删 / 探测

// UpdateCluster 编辑集群：改名称、备注，或换一份 kubeconfig。
//
// kubeconfig / context 留空表示"不修改凭据"——前端只看得到"已配置"，
// 回传不了原文；若把空串当"清空"，那么每次改个备注都会顺手把凭据删掉。
func (s *SystemService) UpdateCluster(ctx context.Context, key string, in ClusterUpdateInput, c Caller) (*K8sClusterDTO, error) {
	row, err := s.clusterByKey(ctx, key)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{"updated_by": c.UserID(), "updated_at": now()}
	if v := strings.TrimSpace(in.Name); v != "" {
		updates["name"] = v
	}
	if in.Remark != nil {
		updates["remark"] = strings.TrimSpace(*in.Remark)
	}

	raw := strings.TrimSpace(in.Kubeconfig)
	contextName := strings.TrimSpace(in.Context)
	if raw != "" || contextName != "" {
		if raw == "" {
			return nil, errs.Invalid("更换 context 时必须同时提供完整 kubeconfig（平台不保存原文，无法只换 context）")
		}
		// 换凭据 = 换一个集群主体，必须**重新探测**并把上一轮的探测结果全部重写。
		// 否则会出现"凭据已经换成另一个集群、版本号与节点数还是旧集群的"，
		// 而这种残留数据在页面上看不出任何异常。
		if contextName == "" {
			contextName = row.ContextName
		}
		minimal, err := integration.MinimalKubeconfig(raw, contextName)
		if err != nil {
			return nil, errs.Invalid(err.Error())
		}
		probe := integration.Probe(ctx, []byte(minimal), s.clusterProbeTimeout())
		if !probe.OK {
			return nil, errs.Invalidf("换凭据失败，集群探测未通过：%s", probe.Message)
		}
		apiServer, kubeCluster := integration.ContextInfoOf(raw, contextName)

		// 凭据与集群行必须同一个事务：否则可能出现"凭据已换成新集群、行上的
		// api_server 还是旧的"，两者指向的集群不同，排障时会互相误导。
		if err := s.tx(ctx, func(tx *gorm.DB) error {
			credID, err := sealKubeconfigCredential(tx, s.deps.Sealer, clusterCredScope, key, minimal, row.Name, c.UserID())
			if err != nil {
				return err
			}
			updates["kubeconfig_credential_id"] = credID
			updates["context_name"] = contextName
			updates["kubeconfig_cluster"] = kubeCluster
			updates["api_server"] = apiServer
			copyProbeUpdates(updates, probe)
			return tx.Model(&model.K8sCluster{}).Where("id = ?", row.ID).Updates(updates).Error
		}); err != nil {
			return nil, wrapDB(err, "更新集群")
		}
	} else if err := s.db(ctx).Model(&model.K8sCluster{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
		return nil, wrapDB(err, "更新集群")
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "cluster:update", TargetType: "k8s_cluster",
		TargetID: &row.ID, TargetName: row.Name, Detail: "编辑 K8s 集群 " + row.Key,
	})
	fresh, err := s.clusterByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	used, err := s.clusterUsedBy(ctx)
	if err != nil {
		return nil, err
	}
	dto := clusterToDTO(fresh, used[fresh.Key])
	return &dto, nil
}

// DeleteCluster 删除集群。**被环境引用时拒绝**，并把引用者一并告诉用户。
//
// 只软删（保留行以继续占用 uk_cluster_key），因此同一个标识改天可以复活
// （见 persistCluster 里的捞回软删逻辑）。
func (s *SystemService) DeleteCluster(ctx context.Context, key string, c Caller) error {
	row, err := s.clusterByKey(ctx, key)
	if err != nil {
		return err
	}
	used, err := s.clusterUsedBy(ctx)
	if err != nil {
		return err
	}
	if refs := used[row.Key]; len(refs) > 0 {
		return errs.StateConflict(fmt.Sprintf(
			"集群 %s 正被 %d 个环境引用（%s），请先把这些环境改到别的集群或删除它们",
			row.Key, len(refs), strings.Join(refs, "、")))
	}
	err = s.tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ?", row.ID).Delete(&model.K8sCluster{}).Error; err != nil {
			return wrapDB(err, "删除集群")
		}
		// 凭据一并清掉。留着一条再也用不上的密文只是负担——
		// 它的唯一用途就是给这个集群建连接。
		if err := tx.Where("scope = ? AND name = ?", clusterCredScope, row.Key).
			Delete(&model.SecretCredential{}).Error; err != nil {
			return wrapDB(err, "清理集群凭据")
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "cluster:delete", TargetType: "k8s_cluster",
		TargetID: &row.ID, TargetName: row.Name, Detail: "删除 K8s 集群 " + row.Key,
	})
	return nil
}

// TestCluster 单集群连通性探测，并回填结果。
//
// `ok=false` 也返回 200（业务成功、结论为不通）——与 GitLab / Jenkins 的
// 测试接口同一约定，前端不能只靠 catch 判断。
func (s *SystemService) TestCluster(ctx context.Context, key string, c Caller) (*ClusterTestResultDTO, error) {
	row, err := s.clusterByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	raw, err := s.clusterKubeconfig(ctx, row)
	if err != nil {
		return nil, err
	}
	probe := integration.Probe(ctx, raw, s.clusterProbeTimeout())

	updates := map[string]any{
		"status":             probe.Status,
		"last_probe_message": probeMessage(probe),
		"last_probe_ms":      probe.LatencyMS,
		"last_probed_at":     now(),
		"updated_at":         now(),
	}
	copyProbeUpdates(updates, probe)
	if err := s.db(ctx).Model(&model.K8sCluster{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
		return nil, wrapDB(err, "回填集群探测结果")
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "cluster:test", TargetType: "k8s_cluster",
		TargetID: &row.ID, TargetName: row.Name,
		Detail: fmt.Sprintf("探测 K8s 集群 %s：%s", row.Key, probe.Message),
		Result: boolResult(probe.OK),
	})
	return &ClusterTestResultDTO{
		Key:            row.Key,
		OK:             probe.OK,
		Message:        probe.Message,
		LatencyMS:      probe.LatencyMS,
		K8sVersion:     probe.K8sVersion,
		Nodes:          probe.Nodes,
		NodesReady:     probe.NodesReady,
		NamespaceCount: probe.NamespaceCount,
		Warnings:       nonNilSlice(probe.Warnings),
	}, nil
}

// ListClusterNamespaces 列命名空间名，供新建环境时选（避免手输写错）。
//
// 集群不可达时**返回错误**而不是空数组：空数组会被前端当成"这个集群没有命名空间"，
// 而事实是"没问到"。前端在出错时会自动退化成可自由输入，不会把表单卡死。
func (s *SystemService) ListClusterNamespaces(ctx context.Context, key string) ([]string, error) {
	row, err := s.clusterByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	raw, err := s.clusterKubeconfig(ctx, row)
	if err != nil {
		return nil, err
	}
	ns, err := integration.ListNamespaces(ctx, raw, s.clusterProbeTimeout())
	if err != nil {
		return nil, errs.Upstream(err, "读取集群 %s 的命名空间失败", row.Key)
	}
	return ns, nil
}

// --------------------------------------------------------------------------- 内部工具

// clusterByKey 按标识取集群（未软删的）。
func (s *SystemService) clusterByKey(ctx context.Context, key string) (*model.K8sCluster, error) {
	row, err := s.loadClusterByKey(s.db(ctx), key)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errs.NotFoundf("集群 %s 不存在", key)
	}
	return row, nil
}

// loadClusterByKey 按标识查集群。未命中返回 (nil, nil)——
// 与"查询失败"区分开：前者是正常的业务分支（标识可用），后者才是异常。
func (s *SystemService) loadClusterByKey(db *gorm.DB, key string) (*model.K8sCluster, error) {
	var row model.K8sCluster
	err := db.Where("cluster_key = ?", key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, errs.Internalf(err, "查询集群失败")
	}
	return &row, nil
}

// clusterKubeconfig 取集群的 kubeconfig 明文（解密）。
// **只在服务端内部流转**，绝不进入任何 DTO。
func (s *SystemService) clusterKubeconfig(ctx context.Context, row *model.K8sCluster) ([]byte, error) {
	if row.CredID == nil {
		return nil, errs.Invalidf("集群 %s 没有已保存的 kubeconfig，请先导入凭据", row.Key)
	}
	var cred model.SecretCredential
	if err := s.db(ctx).First(&cred, *row.CredID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 凭据行被谁删了。这不是"集群没凭据"，是数据不一致，要明说。
			return nil, errs.Internalf(nil, "集群 %s 的凭据记录已丢失，请重新导入 kubeconfig", row.Key)
		}
		return nil, errs.Internalf(err, "查询集群凭据失败")
	}
	plain, err := s.deps.Sealer.Open(crypto.Sealed{Cipher: cred.Cipher, IV: cred.IV, AuthTag: cred.AuthTag})
	if err != nil {
		return nil, errs.Internalf(err, "集群 %s 的 kubeconfig 解密失败（security.masterKey 是否被更换过？）", row.Key)
	}
	return []byte(plain), nil
}

// copyProbeUpdates 把探测结果写进 updates map。字段名要与模型列名一致。
func copyProbeUpdates(updates map[string]any, p *integration.ClusterProbe) {
	updates["k8s_version"] = p.K8sVersion
	updates["nodes"] = p.Nodes
	updates["nodes_ready"] = p.NodesReady
	updates["namespace_count"] = p.NamespaceCount
}

// probeMessage 拼出给用户看的结论。
//
// 成功时只说"集群连通"，**不带版本号**：版本号有 k8sVersion 字段承载，
// 而这里要回答的是"通不通"。失败时把 warnings 接在后面，
// 因为"连通但无权列节点"这类情况只有两句话合起来才完整。
func probeMessage(p *integration.ClusterProbe) string {
	msg := p.Message
	if len(p.Warnings) > 0 {
		msg = msg + "；" + strings.Join(p.Warnings, "；")
	}
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return msg
}

func boolResult(ok bool) string {
	if ok {
		return "success"
	}
	return "failed"
}

// nonNilSlice 把 nil 切片变成空切片，**让"列表字段"在 JSON 里永远是 `[]` 而不是 `null`**。
//
// 这不是洁癖。Go 的 nil 切片会序列化成 `null`，而前端把这类字段声明成 `string[]`，
// 于是 `x.length` 直接抛 TypeError —— 一个渲染异常会卸载整个 React 树，页面全白。
// 曾经这里叫 emptyToNil（方向反了，把空数组变成 null），实测就是这样让
// 集群接入页在"有集群但没被环境引用"时白屏的。
//
// 约定：**列表字段的空值就是空列表**。`null` 只用来表达"没读到 / 未知"
// （如 `nodes *int`），不用来表达"空"。
func nonNilSlice(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// clusterToDTO 模型 → 对外 DTO。**凭据只出布尔**，见本文件顶部约束 1。
func clusterToDTO(m *model.K8sCluster, usedBy []string) K8sClusterDTO {
	return K8sClusterDTO{
		Key:               m.Key,
		Name:              m.Name,
		APIServer:         m.APIServer,
		Context:           m.ContextName,
		KubeconfigCluster: m.KubeconfigCluster,
		K8sVersion:        m.K8sVersion,
		Nodes:             m.Nodes,
		NodesReady:        m.NodesReady,
		NamespaceCount:    m.NamespaceCount,
		Status:            m.Status,
		LastProbedAt:      formatTimePtr(m.LastProbedAt),
		LastProbeMessage:  m.LastProbeMessage,
		LastProbeMS:       probeMS(m.LastProbeMS),
		HasKubeconfig:     m.CredID != nil,
		UsedByEnvs:        nonNilSlice(usedBy),
		Remark:            m.Remark,
		CreatedAt:         formatTime(m.CreatedAt),
		UpdatedAt:         formatTime(m.UpdatedAt),
	}
}

func probeMS(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
