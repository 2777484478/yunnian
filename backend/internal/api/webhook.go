package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/service"
)

// maxWebhookBody 回调体积上限。
//
// 平台并不需要完整 payload（只取仓库、分支、提交三个字段），
// 但有些平台会把全部 commits 塞进来（大仓库一次推送几十 MB）。
// 限制在 2MB 并明确报错，好过让一个畸形请求把内存吃掉。
const maxWebhookBody = 2 << 20

// webhookGitLab POST /webhooks/gitlab
//
// 鉴权靠共享密钥（X-Gitlab-Token）而不是 JWT：调用方是 GitLab 服务端，
// 它拿不到也不能持有用户令牌。密钥未配置时**直接拒绝**——
// 一个"没配密钥就放行"的 webhook 等于给公网开了一个免鉴权的构建触发器。
func (h *handler) webhookGitLab(c *gin.Context) {
	secret := strings.TrimSpace(h.cfg.Integration.Webhook.GitLabSecret)
	if secret == "" {
		httpx.Fail(c, errs.New(errs.CodeForbidden,
			"未配置 integration.webhook.gitlabSecret，已拒绝该回调（不允许无签名触发构建）"))
		return
	}
	// 用 ConstantTimeCompare 而不是 == ：普通字符串比较会在第一个不同字节处返回，
	// 攻击者能靠耗时差异逐字节猜出密钥。
	if !hmac.Equal([]byte(c.GetHeader("X-Gitlab-Token")), []byte(secret)) {
		httpx.Fail(c, errs.Unauthorized("webhook 令牌校验失败"))
		return
	}

	var payload struct {
		Ref       string `json:"ref"`
		After     string `json:"after"`
		Project   struct {
			GitHTTPURL string `json:"git_http_url"`
			GitSSHURL  string `json:"git_ssh_url"`
			WebURL     string `json:"web_url"`
			PathNS     string `json:"path_with_namespace"`
		} `json:"project"`
		Commits []struct {
			ID      string `json:"id"`
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"commits"`
	}
	if !h.bindWebhookJSON(c, &payload) {
		return
	}

	commitID := strings.TrimSpace(payload.After)
	commitMsg, author := "", ""
	if len(payload.Commits) > 0 {
		last := payload.Commits[len(payload.Commits)-1]
		commitMsg, author = firstLine(last.Message), strings.TrimSpace(last.Author.Name)
		if commitID == "" {
			commitID = last.ID
		}
	}
	// 全零 SHA 表示"删除分支"这类无提交的 ref 变更，不应触发构建。
	if commitID == zeroSHA || commitID == "" {
		httpx.OK(c, gin.H{"matched": false, "reason": "非提交推送（可能是分支删除），已忽略"})
		return
	}

	out, err := h.svc.CI.HandlePush(c.Request.Context(), service.HandlePushInput{
		Provider:  "gitlab",
		GitURL:    stringOr(payload.Project.GitHTTPURL, payload.Project.GitSSHURL),
		Branch:    trimRefPrefix(payload.Ref),
		CommitID:  commitID,
		CommitMsg: commitMsg,
		Author:    author,
	})
	h.writePushOutcome(c, out, err)
}

// webhookGitHub POST /webhooks/github
//
// GitHub 用 HMAC-SHA256 签名（X-Hub-Signature-256），签名内容必须是对
// **原始请求体** 做 HMAC——所以这里先整段读进内存再验签，
// 不能先 ShouldBindJSON 再拿结构体的重新序列化结果去算（字段顺序与空格都会变）。
func (h *handler) webhookGitHub(c *gin.Context) {
	secret := strings.TrimSpace(h.cfg.Integration.Webhook.GitHubSecret)
	if secret == "" {
		httpx.Fail(c, errs.New(errs.CodeForbidden,
			"未配置 integration.webhook.githubSecret，已拒绝该回调（不允许无签名触发构建）"))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody))
	if err != nil {
		httpx.Fail(c, errs.Invalid("读取请求体失败"))
		return
	}
	sig := strings.TrimSpace(c.GetHeader("X-Hub-Signature-256"))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		httpx.Fail(c, errs.Unauthorized("webhook 签名校验失败"))
		return
	}

	var payload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			CloneURL string `json:"clone_url"`
			SSHURL   string `json:"ssh_url"`
			HTMLURL  string `json:"html_url"`
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadCommit *struct {
			ID      string `json:"id"`
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		httpx.Fail(c, errs.Invalid("回调载荷解析失败"))
		return
	}

	commitID, commitMsg, author := strings.TrimSpace(payload.After), "", ""
	if payload.HeadCommit != nil {
		if payload.HeadCommit.ID != "" {
			commitID = payload.HeadCommit.ID
		}
		commitMsg = firstLine(payload.HeadCommit.Message)
		author = strings.TrimSpace(payload.HeadCommit.Author.Name)
	}
	if commitID == zeroSHA || commitID == "" {
		httpx.OK(c, gin.H{"matched": false, "reason": "非提交推送（可能是分支删除），已忽略"})
		return
	}

	out, err := h.svc.CI.HandlePush(c.Request.Context(), service.HandlePushInput{
		Provider:  "github",
		GitURL:    stringOr(payload.Repository.CloneURL, payload.Repository.SSHURL),
		Branch:    trimRefPrefix(payload.Ref),
		CommitID:  commitID,
		CommitMsg: commitMsg,
		Author:    author,
	})
	h.writePushOutcome(c, out, err)
}

// zeroSHA GitLab/GitHub 用于表示"该 ref 已不存在"的全零哈希。
const zeroSHA = "0000000000000000000000000000000000000000"

// bindWebhookJSON 读取并解析回调体，带体积上限。
func (h *handler) bindWebhookJSON(c *gin.Context, out any) bool {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody))
	if err != nil {
		httpx.Fail(c, errs.Invalid("读取请求体失败"))
		return false
	}
	if err := json.Unmarshal(raw, out); err != nil {
		httpx.Fail(c, errs.Invalid("回调载荷解析失败"))
		return false
	}
	return true
}

// writePushOutcome 写出推送处理结果。
//
// "没匹配到项目"也返回 200：这是正常业务结局（比如推了一个未纳管的仓库），
// 返回 4xx 会让代码托管平台把回调标成失败并不断重试。
func (h *handler) writePushOutcome(c *gin.Context, out *service.PushOutcome, err error) {
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if out == nil {
		httpx.OK(c, gin.H{"matched": false, "reason": "无处理结果"})
		return
	}
	httpx.OK(c, out)
}

// trimRefPrefix 把 refs/heads/master 还原成 master。
func trimRefPrefix(ref string) string {
	v := strings.TrimSpace(ref)
	for _, p := range []string{"refs/heads/", "refs/tags/"} {
		if strings.HasPrefix(v, p) {
			return strings.TrimPrefix(v, p)
		}
	}
	return v
}

// firstLine 取提交信息首行：完整 message 可能很长，且入库字段有长度限制。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// stringOr 空则取备选值。
func stringOr(v, def string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	return v
}
