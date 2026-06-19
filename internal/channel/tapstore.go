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

// tapstore.go — TapStore の実装。InMemory(テスト用)と GitHub(実体)。

// InMemoryTapStore はテスト用。tap の内容をメモリに持つ(末端の差し替え・01)。
type InMemoryTapStore struct {
	Files   map[string]string
	Commits int // Put 回数(書き込み発生の検証用)
}

func NewInMemoryTapStore() *InMemoryTapStore {
	return &InMemoryTapStore{Files: map[string]string{}}
}

func (s *InMemoryTapStore) Get(_ context.Context, path string) (string, bool, error) {
	c, ok := s.Files[path]
	return c, ok, nil
}

func (s *InMemoryTapStore) Put(_ context.Context, path, content, _ string) (string, error) {
	s.Files[path] = content
	s.Commits++
	return fmt.Sprintf("inmem%d", s.Commits), nil
}

// GitHubTapStore は GitHub Contents API 経由の実体(owned tap への読み書き)。
// 読みは未認証でも公開 tap なら通る。書きには Token(GITHUB_TOKEN)が要る。
type GitHubTapStore struct {
	Owner, Repo string
	Token       string // 書き込みに必要。空なら Put は失敗
	Branch      string // 既定 ""(リポジトリ既定ブランチ)
	API         string // 既定 https://api.github.com
	HTTP        *http.Client
}

func NewGitHubTapStore(owner, repo, token string) *GitHubTapStore {
	return &GitHubTapStore{Owner: owner, Repo: repo, Token: token, API: "https://api.github.com", HTTP: http.DefaultClient}
}

func (s *GitHubTapStore) contentsURL(path string) string {
	return fmt.Sprintf("%s/repos/%s/%s/contents/%s", s.api(), s.Owner, s.Repo, path)
}

func (s *GitHubTapStore) api() string {
	if s.API == "" {
		return "https://api.github.com"
	}
	return s.API
}

func (s *GitHubTapStore) client() *http.Client {
	if s.HTTP == nil {
		return http.DefaultClient
	}
	return s.HTTP
}

type ghContent struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	SHA      string `json:"sha"`
}

// Get は contents API で path を取得し、base64 を解いて返す。404 は found=false。
func (s *GitHubTapStore) Get(ctx context.Context, path string) (string, bool, error) {
	content, _, found, err := s.getWithSHA(ctx, path)
	return content, found, err
}

func (s *GitHubTapStore) getWithSHA(ctx context.Context, path string) (content, sha string, found bool, err error) {
	url := s.contentsURL(path)
	if s.Branch != "" {
		url += "?ref=" + s.Branch
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", false, err
	}
	s.auth(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.client().Do(req)
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", false, nil
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", false, fmt.Errorf("github get %s: %s: %s", path, resp.Status, snippet(body))
	}
	var c ghContent
	if err := json.Unmarshal(body, &c); err != nil {
		return "", "", false, err
	}
	dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
	if err != nil {
		return "", "", false, err
	}
	return string(dec), c.SHA, true, nil
}

// Put は contents API で path を作成/更新する。更新時は既存 sha を渡す(冪等)。
func (s *GitHubTapStore) Put(ctx context.Context, path, content, message string) (string, error) {
	if s.Token == "" {
		return "", fmt.Errorf("GITHUB_TOKEN required to write to tap %s/%s", s.Owner, s.Repo)
	}
	_, sha, _, err := s.getWithSHA(ctx, path)
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	}
	if sha != "" {
		payload["sha"] = sha
	}
	if s.Branch != "" {
		payload["branch"] = s.Branch
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.contentsURL(path), bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	s.auth(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github put %s: %s: %s", path, resp.Status, snippet(body))
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Commit.SHA, nil
}

func (s *GitHubTapStore) auth(req *http.Request) {
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
}

func snippet(b []byte) string {
	const n = 200
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}
