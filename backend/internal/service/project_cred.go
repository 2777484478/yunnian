package service

import (
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yunnian/yunnian/internal/crypto"
	"github.com/yunnian/yunnian/internal/errs"
	"github.com/yunnian/yunnian/internal/integration"
	"github.com/yunnian/yunnian/internal/model"
)

// --------------------------------------------------------------------------- 项目级 Git 凭据
//
// 为什么要按项目存一份，而不是复用「系统管理 › 第三方集成」里那个平台级 GitLab 通道：
//
//   - 那个通道是**一个开关管全部项目**，关掉之后所有项目的构建都会静默退化成匿名检出
//     （公开仓库没事，私有仓库报 401，而报错指向的是"仓库地址不对"）；
//   - 不同仓库可能在**不同的 GitLab** 上、用**不同的机器人账号**，一份配置表达不了；
//   - "测试连接"这个动作天然属于某个具体仓库，平台级配置回答不了"我要连的是哪个"。
//
// 账号明文存在 project.git_username（它不是秘密，git remote 里就有），
// 密码走 secret_credential 加密，project.git_credential_id 指过去。

const (
	// projectCredScope 凭据归属域。与 k8s_cluster / environment 并列，
	// 同一个标识在不同 scope 下各存各的，不会互相覆盖。
	projectCredScope = "project"
	// projectGitCredType 凭据类型，与 model.SecretCredential.Type 的既有取值同族
	// （git_token / harbor_robot / kubeconfig / ssh_key / webhook_secret）。
	projectGitCredType = "git_token"
)

// bindProjectGitCredential 把项目密码加密落库并绑定到 project.git_credential_id。
//
// password 为空表示**不修改**（不是清空）：界面回传不了原文，若把空串当清空，
// 那么"只改了个负责人"就会顺手把密码删掉，下一次构建才报 401。
//
// 请在事务里调用：凭据写入与项目行更新必须同生共死。
func (s *CIService) bindProjectGitCredential(tx *gorm.DB, p *model.Project, password string, userID *uint64) error {
	if password == "" {
		return nil
	}
	if strings.TrimSpace(p.ProjectKey) == "" {
		return errs.Invalid("项目标识为空，无法保存仓库凭据")
	}
	sealed, err := s.deps.Sealer.Seal(password)
	if err != nil {
		return errs.Internalf(err, "加密仓库凭据失败")
	}

	var cred model.SecretCredential
	findErr := tx.Where("scope = ? AND name = ?", projectCredScope, p.ProjectKey).First(&cred).Error
	switch {
	case findErr == nil:
		if uerr := tx.Model(&model.SecretCredential{}).Where("id = ?", cred.ID).Updates(map[string]any{
			"type":     projectGitCredType,
			"cipher":   sealed.Cipher,
			"iv":       sealed.IV,
			"auth_tag": sealed.AuthTag,
			// 描述每次都按当前仓库名重写：换了仓库名之后还留着旧描述，
			// 会让人以为这条凭据属于另一个项目。
			"description": projectCredDescription(p),
			"updated_at":  now(),
		}).Error; uerr != nil {
			return wrapDB(uerr, "更新仓库凭据")
		}
		p.GitCredential = &cred.ID
	case errors.Is(findErr, gorm.ErrRecordNotFound):
		cred = model.SecretCredential{
			// Name 用项目标识而不是显示名：唯一键是 (scope, name)，
			// 用显示名的话改个中文名就会多出一条新凭据、旧的永远留着。
			Name:        p.ProjectKey,
			Type:        projectGitCredType,
			Scope:       projectCredScope,
			Cipher:      sealed.Cipher,
			IV:          sealed.IV,
			AuthTag:     sealed.AuthTag,
			KeyVersion:  1,
			Description: projectCredDescription(p),
			CreatedBy:   userID,
		}
		if cerr := tx.Create(&cred).Error; cerr != nil {
			return wrapDB(cerr, "保存仓库凭据")
		}
		p.GitCredential = &cred.ID
	default:
		return wrapDB(findErr, "查询仓库凭据")
	}

	if uerr := tx.Model(&model.Project{}).Where("id = ?", p.ID).
		Update("git_credential_id", *p.GitCredential).Error; uerr != nil {
		return wrapDB(uerr, "绑定仓库凭据")
	}
	return nil
}

func projectCredDescription(p *model.Project) string {
	if url := strings.TrimSpace(p.GitURL); url != "" {
		return fmt.Sprintf("项目 %s 拉取代码用的凭据（%s）", p.ProjectKey, url)
	}
	return fmt.Sprintf("项目 %s 拉取代码用的凭据", p.ProjectKey)
}

// openProjectGitCredential 解出某个项目拉代码用的账号密码。
//
// 返回空密码是**合法**的：项目没配凭据、或配的是公开仓库，都走匿名检出。
// 调用方必须把这种情况当"匿名"处理，而不是当失败。
//
// ⚠️ 返回的密码只能交给 git，不得进入任何 DTO、日志或审计明细。
func (s *CIService) openProjectGitCredential(ctx context.Context, p *model.Project) (string, string, error) {
	user := strings.TrimSpace(p.GitUsername)
	if p.GitCredential == nil {
		return user, "", nil
	}
	var cred model.SecretCredential
	err := s.db(ctx).First(&cred, *p.GitCredential).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 凭据行没了（手工删过库、或换了主密钥重建）。这必须说出来：
		// 悄悄退回匿名检出，用户看到的会是"拉取代码失败"，而根因是凭据丢了。
		return user, "", fmt.Errorf("项目 %s 的仓库凭据记录已丢失，请到项目编辑里重新填写密码", p.ProjectKey)
	}
	if err != nil {
		return user, "", wrapDB(err, "查询仓库凭据")
	}
	plain, oerr := s.deps.Sealer.Open(crypto.Sealed{Cipher: cred.Cipher, IV: cred.IV, AuthTag: cred.AuthTag})
	if oerr != nil {
		return user, "", fmt.Errorf("项目 %s 的仓库凭据解密失败（security.masterKey 是否被更换过？）", p.ProjectKey)
	}
	return user, plain, nil
}

// --------------------------------------------------------------------------- 测试连接

// TestGitConnectionInput 测试连接的入参。
//
// 两种来源：带 projectKey 表示"用已保存的凭据测一个地址"（界面上改完地址先测一下，
// 不用先保存）；直接带地址与账号密码表示"测这套还没保存的凭据"。
type TestGitConnectionInput struct {
	ProjectKey  string `json:"projectKey" binding:"omitempty,max=64"`
	GitURL      string `json:"gitUrl" binding:"omitempty,max=255"`
	GitUsername string `json:"gitUsername" binding:"omitempty,max=128"`
	GitPassword string `json:"gitPassword" binding:"omitempty,max=512"`
	// Branch 可选：指定时顺带核实这个分支存在，这是"构建必失败"的第一大来源。
	Branch string `json:"branch" binding:"omitempty,max=96"`
}

// TestGitConnectionResult 测试连接结果。
type TestGitConnectionResult struct {
	OK bool `json:"ok"`
	// Message 给用户看的一句话结论。**必须是人话**：把 git 的原始 stderr
	// 直接端出去，用户看到的是 `fatal: Authentication failed for 'http://…'`，
	// 他第一反应会是"平台坏了"而不是"密码错了"。
	Message string `json:"message"`
	// Detail 原始输出尾部。只在失败时给，供运维定位；不含凭据（构造时就已打码）。
	Detail string `json:"detail,omitempty"`
	// Branches 读到的分支名（**最多 branchesLimit 个**，见 BranchTotal）。
	Branches []string `json:"branches"`
	// BranchTotal 远端**实际**有多少个分支。
	//
	// 必须单独给：Branches 会被截到 branchesLimit，用它的长度当"远端分支数"是句假话
	// （实测踩过：仓库真有 229 个分支，提示却告诉用户"远端有 100 个"）。
	// 界面据此说明"共 N 个，已列出前 M 个"。
	BranchTotal int `json:"branchTotal"`
	// DefaultBranch 远端 HEAD 指向的分支，供界面一键填「默认分支」。
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// ElapsedMS 这次探测实际花了多久。用户判断"是慢还是不通"的依据。
	ElapsedMS int64 `json:"elapsedMs"`
}

// 探测超时。给 20 秒：跨公网 + 首次 DNS + TLS 握手，10 秒在慢网络下会误报不通，
// 而"明明能连却说连不上"会让用户去改一个本来正确的地址。
const gitProbeTimeout = 20 * time.Second

// TestGitConnection 真的去读一次仓库。
//
// 为什么不做成"格式校验"：格式永远是对的（`http://…git` 谁都写得出来），
// 用户点这个按钮想知道的恰恰是"这个地址 + 这套凭据，**现在**能不能拉到代码"。
// 只有真连一次才算数。
func (s *CIService) TestGitConnection(ctx context.Context, in TestGitConnectionInput, c Caller) (*TestGitConnectionResult, error) {
	// 没带密码时回落到已保存的凭据：界面上"改完地址点一下测试"不该逼用户重敲密码。
	user := strings.TrimSpace(in.GitUsername)
	pass := in.GitPassword
	url := strings.TrimSpace(in.GitURL)

	if key := strings.TrimSpace(in.ProjectKey); key != "" {
		p, err := s.projectByKey(ctx, key)
		if err != nil {
			return nil, err
		}
		if url == "" {
			url = p.GitURL
		}
		if pass == "" {
			su, sp, cerr := s.openProjectGitCredential(ctx, p)
			if cerr != nil {
				return nil, errs.Internalf(cerr, "读取项目仓库凭据失败")
			}
			if user == "" {
				user = su
			}
			pass = sp
		}
	}

	if err := validateGitURL(url); err != nil {
		return nil, err
	}

	probeCtx, cancel := context.WithTimeout(ctx, gitProbeTimeout)
	defer cancel()

	started := now()
	probe := s.deps.Up.ProbeGit(probeCtx, integration.RepositoryProbe{
		URL:      url,
		Username: user,
		Password: pass,
		Branch:   strings.TrimSpace(in.Branch),
	})

	res := &TestGitConnectionResult{
		OK:        probe.OK,
		Message:   probe.Message,
		Detail:    probe.Detail,
		ElapsedMS: time.Since(started).Milliseconds(),
	}

	full := probe.Branches
	if full == nil {
		// 一律给数组：前端拿到 null 会因为 `.map` 直接崩掉整页。
		full = []string{}
	}
	res.BranchTotal = len(full)

	// 先赋上（并截断），保证**任何** return 路径下 Branches 都不是 nil。
	res.Branches = full
	if len(res.Branches) > branchesLimit {
		res.Branches = res.Branches[:branchesLimit]
	}

	// ⚠️ 分支存在性必须拿**完整列表**判，而且必须在截断之前判。
	//
	// 从前这里先截到 branchesLimit 再判，于是分支数超过上限的仓库会说出两句假话：
	//   - 明明存在的分支被判成"不存在"（它只是排在第 100 名之后）；
	//   - 提示里的"远端有 %d 个分支"报的是**截断后**的数。
	// 用户据此会把一个正确的分支名改掉 —— 而这正是他点"测试连接"想解决的问题。
	// 实测：demowarning 远端有 229 个分支（大写开头的排在前面），
	// 任何小写开头的分支（dev、feature_*）都落在 100 名之外，全被判成"不存在"。
	if res.OK {
		if b := strings.TrimSpace(in.Branch); b != "" && !containsString(full, b) {
			res.OK = false
			res.Message = fmt.Sprintf("地址与凭据都通，但分支 %q 在远端找不到（远端共 %d 个分支）", b, len(full))
			s.logTestConnection(c, url, user, res)
			return res, nil
		}
		res.DefaultBranch = probe.DefaultBranch
	}

	s.logTestConnection(c, url, user, res)
	return res, nil
}

// logTestConnection 记录一次探测。凭据绝不进日志：这条日志将来会被翻出来排查
// "谁连了这个仓库"，而它不需要知道密码就能回答那个问题。
//
// 抽成函数是因为它有**两条**返回路径（分支不存在会提前 return）——
// 从前那条路径直接 return，于是"用户试了个不存在的分支"这件事一次都没被记过。
func (s *CIService) logTestConnection(c Caller, url, user string, res *TestGitConnectionResult) {
	s.log("ci").Info("测试仓库连接",
		"project", c.Name(), "url", redactGitURL(url), "user", user, "ok", res.OK,
		"branchTotal", res.BranchTotal, "elapsedMs", res.ElapsedMS)
}

// branchesLimit 最多回多少个分支给界面。
//
// 上千个分支的仓库确实存在，而"读到 1247 个分支"和列出 1247 行对用户没有区别；
// 但把上千个字符串塞进一个 JSON 响应、再渲染进一个下拉，是有区别的。
const branchesLimit = 100

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// validateGitURL 只做"能不能拿去连"的最低限度检查。
//
// 不校验"存在与否"——那正是测试连接要回答的问题，在这里提前拦会把
// "地址写得对但仓库不存在"变成一句语法错误提示，反而看不清。
func validateGitURL(url string) error {
	if strings.TrimSpace(url) == "" {
		return errs.Invalid("请先填写仓库地址")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") &&
		!strings.Contains(url, "@") {
		return errs.Invalid("仓库地址需为 http(s):// 或 git@host:group/repo 形式")
	}
	return nil
}

// redactGitURL 去掉 URL 里可能内嵌的凭据，只留主机与路径。
//
// `http://user:token@host/x.git` 这种地址是用户实际会填的形态（尤其是从
// GitLab 页面复制的时候），而它会被写进日志与审计明细。
func redactGitURL(raw string) string {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
