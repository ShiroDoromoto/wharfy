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

// winget_github.go — Submitter の実体。microsoft/winget-pkgs を fork し、ブランチに manifest を
// commit して中央リポジトリへ PR を出す(設計 11A)。マージはしない。冪等を志向する。
//
// 注: 実 PR を Microsoft のリポジトリに出すため自動テストでは検証しない(fake Submitter で
// orchestration を検証する)。実運用でのみ走る経路。

// GitHubWingetSubmitter は GitHub API 経由の Submitter。
type GitHubWingetSubmitter struct {
	Token      string
	Central    string // 既定 microsoft/winget-pkgs
	BaseBranch string // 既定 master
	API        string // 既定 https://api.github.com
	HTTP       *http.Client
}

func NewGitHubWingetSubmitter(token string) *GitHubWingetSubmitter {
	return &GitHubWingetSubmitter{Token: token, Central: "microsoft/winget-pkgs", BaseBranch: "master",
		API: "https://api.github.com", HTTP: http.DefaultClient}
}

func (s *GitHubWingetSubmitter) central() string {
	if s.Central == "" {
		return "microsoft/winget-pkgs"
	}
	return s.Central
}

func (s *GitHubWingetSubmitter) base() string {
	if s.BaseBranch == "" {
		return "master"
	}
	return s.BaseBranch
}

func (s *GitHubWingetSubmitter) Submit(ctx context.Context, in WingetInput, files map[string]string) (string, error) {
	if s.Token == "" {
		return "", fmt.Errorf("GITHUB_TOKEN required to submit a winget PR")
	}
	user, err := s.authedUser(ctx)
	if err != nil {
		return "", err
	}
	fork := user + "/winget-pkgs"

	if err := s.fork(ctx); err != nil {
		return "", fmt.Errorf("fork %s: %w", s.central(), err)
	}
	baseSHA, err := s.refSHA(ctx, fork, "heads/"+s.base())
	if err != nil {
		return "", fmt.Errorf("get base ref (fork may still be initializing): %w", err)
	}
	if err := s.createBranch(ctx, fork, in.BranchName(), baseSHA); err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}
	for name, content := range files {
		path := in.ManifestDir() + name
		if err := s.putFile(ctx, fork, path, content, in.BranchName()); err != nil {
			return "", fmt.Errorf("commit %s: %w", path, err)
		}
	}
	return s.openPR(ctx, user, in)
}

func (s *GitHubWingetSubmitter) authedUser(ctx context.Context) (string, error) {
	var out struct {
		Login string `json:"login"`
	}
	if _, err := s.do(ctx, http.MethodGet, "/user", nil, &out); err != nil {
		return "", err
	}
	return out.Login, nil
}

func (s *GitHubWingetSubmitter) fork(ctx context.Context) error {
	// 既に fork があっても 202/200 が返る(冪等)。
	_, err := s.do(ctx, http.MethodPost, "/repos/"+s.central()+"/forks", map[string]any{}, nil)
	return err
}

func (s *GitHubWingetSubmitter) refSHA(ctx context.Context, repo, ref string) (string, error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := s.do(ctx, http.MethodGet, "/repos/"+repo+"/git/ref/"+ref, nil, &out); err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}

func (s *GitHubWingetSubmitter) createBranch(ctx context.Context, repo, branch, sha string) error {
	status, err := s.do(ctx, http.MethodPost, "/repos/"+repo+"/git/refs",
		map[string]any{"ref": "refs/heads/" + branch, "sha": sha}, nil)
	if status == http.StatusUnprocessableEntity {
		return nil // 既に存在(冪等)
	}
	return err
}

func (s *GitHubWingetSubmitter) putFile(ctx context.Context, repo, path, content, branch string) error {
	payload := map[string]any{
		"message": "wharfy: winget manifest",
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	}
	// 既存ファイルは sha が要る(更新・冪等)。
	if sha := s.fileSHA(ctx, repo, path, branch); sha != "" {
		payload["sha"] = sha
	}
	_, err := s.do(ctx, http.MethodPut, "/repos/"+repo+"/contents/"+path, payload, nil)
	return err
}

func (s *GitHubWingetSubmitter) fileSHA(ctx context.Context, repo, path, branch string) string {
	var out struct {
		SHA string `json:"sha"`
	}
	_, _ = s.do(ctx, http.MethodGet, "/repos/"+repo+"/contents/"+path+"?ref="+branch, nil, &out)
	return out.SHA
}

func (s *GitHubWingetSubmitter) openPR(ctx context.Context, user string, in WingetInput) (string, error) {
	head := user + ":" + in.BranchName()
	body := map[string]any{
		"title": fmt.Sprintf("New version: %s version %s", in.Identifier, in.Version),
		"head":  head,
		"base":  s.base(),
		"body":  "Submitted by wharfy. Manifest for " + in.Identifier + " " + in.Version + ".",
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	status, err := s.do(ctx, http.MethodPost, "/repos/"+s.central()+"/pulls", body, &out)
	if status == http.StatusUnprocessableEntity {
		// 同 head の PR が既にある(冪等): 既存を探して返す。
		if url := s.existingPR(ctx, user, in.BranchName()); url != "" {
			return url, nil
		}
	}
	if err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

func (s *GitHubWingetSubmitter) existingPR(ctx context.Context, user, branch string) string {
	var out []struct {
		HTMLURL string `json:"html_url"`
	}
	_, _ = s.do(ctx, http.MethodGet, "/repos/"+s.central()+"/pulls?head="+user+":"+branch+"&state=open", nil, &out)
	if len(out) > 0 {
		return out[0].HTMLURL
	}
	return ""
}

// do は GitHub API を叩く小さなヘルパ。out!=nil なら 2xx の body を JSON 復号する。
func (s *GitHubWingetSubmitter) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	api := s.API
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
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	cl := s.HTTP
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
