package channel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// github_pr.go — gated チャネル共通の GitHub fork→branch→ファイル commit→PR(設計 11A)。
// winget(microsoft/winget-pkgs)も *-core(Homebrew/homebrew-core 等)もこの 1 経路を使う。
// 中央リポジトリへ PR を出すだけでマージはしない。冪等を志向する(再実行で重複 PR を作らない)。
//
// 実 PR を上流に出すため自動テストでは実体検証しない(各チャネルは fake で orchestration を検証)。
// 低レベルの fork/branch/commit/PR は httptest で検証する(github_pr_test)。

// ghPR は GitHub API 経由で fork PR を組み立てる小さなクライアント。
type ghPR struct {
	Token string
	API   string // 既定 https://api.github.com
	HTTP  *http.Client
}

// submit は central(owner/repo)を fork し、branch に files(リポジトリ相対パス→内容)を commit して
// PR を出す。戻りは PR の URL。同 head の PR が既にあれば既存を返す(冪等)。
func (g *ghPR) submit(ctx context.Context, central, baseBranch, branch string, files map[string]string, message, prTitle, prBody string) (string, error) {
	if g.Token == "" {
		return "", fmt.Errorf("GITHUB_TOKEN required to submit a PR to %s", central)
	}
	user, err := g.authedUser(ctx)
	if err != nil {
		return "", err
	}
	fork := user + "/" + repoName(central)

	if err := g.fork(ctx, central); err != nil {
		return "", fmt.Errorf("fork %s: %w", central, err)
	}
	baseSHA, err := g.refSHA(ctx, fork, "heads/"+baseBranch)
	if err != nil {
		return "", fmt.Errorf("get base ref (fork may still be initializing): %w", err)
	}
	if err := g.createBranch(ctx, fork, branch, baseSHA); err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}
	for path, content := range files {
		if err := g.putFile(ctx, fork, path, content, branch, message); err != nil {
			return "", fmt.Errorf("commit %s: %w", path, err)
		}
	}
	return g.openPR(ctx, central, user+":"+branch, baseBranch, prTitle, prBody)
}

func repoName(ownerRepo string) string {
	if i := strings.LastIndex(ownerRepo, "/"); i >= 0 {
		return ownerRepo[i+1:]
	}
	return ownerRepo
}

func (g *ghPR) authedUser(ctx context.Context) (string, error) {
	var out struct {
		Login string `json:"login"`
	}
	if _, err := g.do(ctx, http.MethodGet, "/user", nil, &out); err != nil {
		return "", err
	}
	return out.Login, nil
}

func (g *ghPR) fork(ctx context.Context, central string) error {
	_, err := g.do(ctx, http.MethodPost, "/repos/"+central+"/forks", map[string]any{}, nil) // 既存でも 2xx(冪等)
	return err
}

func (g *ghPR) refSHA(ctx context.Context, repo, ref string) (string, error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := g.do(ctx, http.MethodGet, "/repos/"+repo+"/git/ref/"+ref, nil, &out); err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}

func (g *ghPR) createBranch(ctx context.Context, repo, branch, sha string) error {
	status, err := g.do(ctx, http.MethodPost, "/repos/"+repo+"/git/refs",
		map[string]any{"ref": "refs/heads/" + branch, "sha": sha}, nil)
	if status == http.StatusUnprocessableEntity {
		return nil // 既に存在(冪等)
	}
	return err
}

func (g *ghPR) putFile(ctx context.Context, repo, path, content, branch, message string) error {
	payload := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	}
	if sha := g.fileSHA(ctx, repo, path, branch); sha != "" {
		payload["sha"] = sha // 既存ファイルは更新(冪等)
	}
	_, err := g.do(ctx, http.MethodPut, "/repos/"+repo+"/contents/"+path, payload, nil)
	return err
}

func (g *ghPR) fileSHA(ctx context.Context, repo, path, branch string) string {
	var out struct {
		SHA string `json:"sha"`
	}
	_, _ = g.do(ctx, http.MethodGet, "/repos/"+repo+"/contents/"+path+"?ref="+branch, nil, &out)
	return out.SHA
}

func (g *ghPR) openPR(ctx context.Context, central, head, base, title, body string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	status, err := g.do(ctx, http.MethodPost, "/repos/"+central+"/pulls",
		map[string]any{"title": title, "head": head, "base": base, "body": body}, &out)
	if status == http.StatusUnprocessableEntity {
		if url := g.existingPR(ctx, central, head); url != "" { // 同 head の PR が既存(冪等)
			return url, nil
		}
	}
	if err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

func (g *ghPR) existingPR(ctx context.Context, central, head string) string {
	var out []struct {
		HTMLURL string `json:"html_url"`
	}
	_, _ = g.do(ctx, http.MethodGet, "/repos/"+central+"/pulls?head="+head+"&state=open", nil, &out)
	if len(out) > 0 {
		return out[0].HTMLURL
	}
	return ""
}

// do は GitHub API を叩く。out!=nil なら 2xx の body を JSON 復号する。
func (g *ghPR) do(ctx context.Context, method, path string, body, out any) (int, error) {
	api := g.API
	if api == "" {
		api = "https://api.github.com"
	}
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, api+path, r)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	cl := g.HTTP
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil {
			_ = json.Unmarshal(rb, out)
		}
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(rb[:min(len(rb), 200)])))
}
