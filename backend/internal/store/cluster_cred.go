package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/crypto"
	"github.com/2777484478/yunnian/internal/model"
)

// LoadClusterKubeconfig 按集群引用解析出 kubeconfig 明文。
//
// 这是**集群操作的唯一凭据入口**。环境可以引用平台已接入集群的凭据，也可以自带一份
// （「新建环境」时选"独立 kubeconfig"）；两条路径都必须落回这里，
// 否则会出现"拓扑页用的这份、部署用的那份"——那种分叉最终一定表现为某个页面连错集群。
//
// 签名刻意用 (clusterKey, envID) 而不是 integration.ClusterRef：
// 数据层不必知道集成层的类型，只在装配处做一次转换。
//
// ⚠️ 返回的是**含私钥的明文**，只能交给 client-go，不得进入任何 DTO、日志或审计明细。
func LoadClusterKubeconfig(ctx context.Context, db *gorm.DB, sealer *crypto.Sealer, clusterKey string, envID *uint64) ([]byte, error) {
	credID, label, err := resolveClusterCredID(ctx, db, clusterKey, envID)
	if err != nil {
		return nil, err
	}
	return openClusterCredential(ctx, db, sealer, credID, label)
}

// resolveClusterCredID 定位该用哪条凭据。返回的 label 只用于拼错误信息。
//
// envID 非 nil 时**优先用环境自带的凭据**，不再看平台集群表：
// 用户建环境时明确选了"独立 kubeconfig"，说明那个集群不在平台统一接入范围内，
// 此时再按 cluster 名去平台表里碰运气，只会连到一个同名的、完全无关的集群。
func resolveClusterCredID(ctx context.Context, db *gorm.DB, clusterKey string, envID *uint64) (credID uint64, label string, err error) {
	if envID != nil {
		var env model.Environment
		qerr := db.WithContext(ctx).Select("id", "env_key", "kubeconfig_credential_id").
			First(&env, *envID).Error
		if errors.Is(qerr, gorm.ErrRecordNotFound) {
			return 0, "", fmt.Errorf("环境 id=%d 不存在", *envID)
		}
		if qerr != nil {
			return 0, "", fmt.Errorf("查询环境失败：%w", qerr)
		}
		if env.KubeCredID == nil {
			// 环境标了用自带凭据，凭据行却没有。这是数据不一致，要明说，
			// 不能悄悄退回平台集群——那会去操作一个完全不相干的集群。
			return 0, "", fmt.Errorf("环境 %s 标记为使用独立 kubeconfig，但没有找到它的凭据，请到环境设置里重新导入", env.EnvKey)
		}
		return *env.KubeCredID, "环境 " + env.EnvKey, nil
	}

	if strings.TrimSpace(clusterKey) == "" {
		return 0, "", errors.New("该环境没有配置集群，请先到「环境管理 › 编辑」里选择集群")
	}
	var row model.K8sCluster
	qerr := db.WithContext(ctx).Select("id", "cluster_key", "kubeconfig_credential_id").
		Where("cluster_key = ?", strings.TrimSpace(clusterKey)).First(&row).Error
	if errors.Is(qerr, gorm.ErrRecordNotFound) {
		return 0, "", fmt.Errorf("集群 %q 未在平台接入：请到「系统管理 › 集群接入」添加后再试", clusterKey)
	}
	if qerr != nil {
		return 0, "", fmt.Errorf("查询集群失败：%w", qerr)
	}
	if row.CredID == nil {
		return 0, "", fmt.Errorf("集群 %q 没有已保存的 kubeconfig，请重新导入凭据", row.Key)
	}
	return *row.CredID, "集群 " + row.Key, nil
}

// openClusterCredential 读凭据行并解密。
func openClusterCredential(ctx context.Context, db *gorm.DB, sealer *crypto.Sealer, credID uint64, label string) ([]byte, error) {
	var cred model.SecretCredential
	err := db.WithContext(ctx).First(&cred, credID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%s 的凭据记录已丢失，请重新导入 kubeconfig", label)
	}
	if err != nil {
		return nil, fmt.Errorf("查询集群凭据失败：%w", err)
	}
	plain, err := sealer.Open(crypto.Sealed{Cipher: cred.Cipher, IV: cred.IV, AuthTag: cred.AuthTag})
	if err != nil {
		// 主密钥换过之后旧密文就解不开了。这是最需要说清楚的一类故障：
		// 不点破的话，用户会去怀疑 kubeconfig 本身有问题。
		return nil, fmt.Errorf("%s 的 kubeconfig 解密失败（security.masterKey 是否被更换过？）", label)
	}
	return []byte(plain), nil
}
