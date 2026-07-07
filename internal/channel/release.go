package channel

// release.go — GitHub Releases への直接アップロード(BYO-binary のネイティブ経路・依頼①)。
//
// GoReleaser を使わず、wharfy が archive 化した成果物と install.sh を Release アセットとして
// 上げる。GoReleaser の prebuilt builder は Pro 専用のため、OSS の wharfy は自前で行う(D-1)。
// tapstore と同じ net/http パターン。tag のリリースを get-or-create し、同名アセットは置換する
// (--yes 再実行を冪等にする)。書き込みには GITHUB_TOKEN が要る。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// ReleaseAsset は Release にアップロードする 1 アセット。
type ReleaseAsset struct {
	Name        string // Release 上のアセット名(例: app_0.1.0_linux_amd64.tar.gz)
	Path        string // アップロード元のローカルパス
	ContentType string // 空なら application/octet-stream
}

// ReleaseStore は tag のリリースを用意しアセットを(置換)アップロードする境界(末端差し替え・01)。
type ReleaseStore interface {
	Upload(ctx context.Context, tag, releaseName string, assets []ReleaseAsset) error
}

// InMemoryReleaseStore はテスト用。tag ごとのアセット名→パスを記録する。
type InMemoryReleaseStore struct {
	Tags     map[string]map[string]string // tag → (name → local path)
	Uploads  int
	Replaced int
}

func NewInMemoryReleaseStore() *InMemoryReleaseStore {
	return &InMemoryReleaseStore{Tags: map[string]map[string]string{}}
}

func (s *InMemoryReleaseStore) Upload(_ context.Context, tag, _ string, assets []ReleaseAsset) error {
	m := s.Tags[tag]
	if m == nil {
		m = map[string]string{}
		s.Tags[tag] = m
	}
	for _, a := range assets {
		if _, ok := m[a.Name]; ok {
			s.Replaced++
		}
		m[a.Name] = a.Path
		s.Uploads++
	}
	return nil
}

// GitHubReleaseStore は GitHub Releases API 経由の実体。
type GitHubReleaseStore struct {
	Owner, Repo string
	Token       string // アップロードに必要
	API         string // 既定 https://api.github.com
	Uploads     string // 既定 https://uploads.github.com(アセットアップロードのホスト)
	HTTP        *http.Client
}

func NewGitHubReleaseStore(owner, repo, token string) *GitHubReleaseStore {
	return &GitHubReleaseStore{
		Owner: owner, Repo: repo, Token: token,
		API:     "https://api.github.com",
		Uploads: "https://uploads.github.com",
		HTTP:    http.DefaultClient,
	}
}

func (s *GitHubReleaseStore) api() string {
	if s.API == "" {
		return "https://api.github.com"
	}
	return s.API
}

func (s *GitHubReleaseStore) uploadsHost() string {
	if s.Uploads == "" {
		return "https://uploads.github.com"
	}
	return s.Uploads
}

func (s *GitHubReleaseStore) client() *http.Client {
	if s.HTTP == nil {
		return http.DefaultClient
	}
	return s.HTTP
}

func (s *GitHubReleaseStore) auth(req *http.Request) {
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
}

type ghRelease struct {
	ID     int64 `json:"id"`
	Assets []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"assets"`
}

// Upload は tag のリリースを get-or-create し、各アセットを(同名は置換して)アップロードする。
func (s *GitHubReleaseStore) Upload(ctx context.Context, tag, releaseName string, assets []ReleaseAsset) error {
	if s.Token == "" {
		return fmt.Errorf("GITHUB_TOKEN required to upload the release to %s/%s", s.Owner, s.Repo)
	}
	rel, err := s.ensureRelease(ctx, tag, releaseName)
	if err != nil {
		return err
	}
	existing := map[string]int64{}
	for _, a := range rel.Assets {
		existing[a.Name] = a.ID
	}
	for _, a := range assets {
		if id, ok := existing[a.Name]; ok {
			if err := s.deleteAsset(ctx, id); err != nil {
				return fmt.Errorf("replace asset %s: %w", a.Name, err)
			}
		}
		if err := s.uploadAsset(ctx, rel.ID, a); err != nil {
			return err
		}
	}
	return nil
}

// ensureRelease は tag のリリースを取得し、無ければ作る。
func (s *GitHubReleaseStore) ensureRelease(ctx context.Context, tag, name string) (*ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", s.api(), s.Owner, s.Repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	s.auth(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		var rel ghRelease
		if err := json.Unmarshal(body, &rel); err != nil {
			return nil, err
		}
		return &rel, nil
	case http.StatusNotFound:
		return s.createRelease(ctx, tag, name)
	default:
		return nil, fmt.Errorf("github get release %s: %s: %s", tag, resp.Status, snippet(body))
	}
}

func (s *GitHubReleaseStore) createRelease(ctx context.Context, tag, name string) (*ghRelease, error) {
	if name == "" {
		name = tag
	}
	payload := map[string]any{"tag_name": tag, "name": name}
	b, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/repos/%s/%s/releases", s.api(), s.Owner, s.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	s.auth(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github create release %s: %s: %s", tag, resp.Status, snippet(body))
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func (s *GitHubReleaseStore) deleteAsset(ctx context.Context, id int64) error {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d", s.api(), s.Owner, s.Repo, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	s.auth(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github delete asset %d: %s", id, resp.Status)
	}
	return nil
}

func (s *GitHubReleaseStore) uploadAsset(ctx context.Context, releaseID int64, a ReleaseAsset) error {
	f, err := os.Open(a.Path)
	if err != nil {
		return fmt.Errorf("open asset %s: %w", a.Name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	url := fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets?name=%s", s.uploadsHost(), s.Owner, s.Repo, releaseID, a.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, f)
	if err != nil {
		return err
	}
	s.auth(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", ct)
	req.ContentLength = info.Size()
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github upload asset %s: %s: %s", a.Name, resp.Status, snippet(body))
	}
	return nil
}

// AssetContentType はアセット名から Content-Type を粗く決める(install.sh はシェル、他は octet-stream)。
func AssetContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".sh"):
		return "text/x-shellscript"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".zip"):
		return "application/zip"
	case strings.HasSuffix(name, ".tar.gz"):
		return "application/gzip"
	default:
		return "application/octet-stream"
	}
}
