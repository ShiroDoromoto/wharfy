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

// ReleaseOptions はリリースを**新しく作るとき**の属性。既に在るリリースには適用しない
// —— 上げ直しが黙って公開状態を切り替えてしまわないため(切り替えは別の工程が明示的に行う)。
type ReleaseOptions struct {
	// Prerelease は prerelease として作る(資産は公開 URL から落ちるが、GitHub の latest にはならない
	// ＝ releases/latest/download/ は旧版を指したまま)。配る実物を、利用者に見せる前に検証する窓。
	Prerelease bool
}

// ReleaseState は tag のリリースの現状(上げる前に、上げ先が何であるかを知るため)。
type ReleaseState struct {
	Exists     bool
	Prerelease bool
}

// ReleaseStore は tag のリリースを用意しアセットを(置換)アップロードする境界(末端差し替え)。
type ReleaseStore interface {
	Upload(ctx context.Context, tag, releaseName string, assets []ReleaseAsset, opt ReleaseOptions) error
	// Get は tag のリリースの現状を返す。無ければ Exists=false(エラーではない)。
	Get(ctx context.Context, tag string) (ReleaseState, error)
}

// InMemoryReleaseStore はテスト用。tag ごとのアセット名→パスを記録する。
type InMemoryReleaseStore struct {
	Tags     map[string]map[string]string // tag → (name → local path)
	Pre      map[string]bool              // tag → prerelease か
	Uploads  int
	Replaced int
}

func NewInMemoryReleaseStore() *InMemoryReleaseStore {
	return &InMemoryReleaseStore{Tags: map[string]map[string]string{}, Pre: map[string]bool{}}
}

func (s *InMemoryReleaseStore) Upload(_ context.Context, tag, _ string, assets []ReleaseAsset, opt ReleaseOptions) error {
	m := s.Tags[tag]
	if m == nil {
		m = map[string]string{}
		s.Tags[tag] = m
		s.Pre[tag] = opt.Prerelease // 作るときだけ属性が効く(既存には触らない)
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

func (s *InMemoryReleaseStore) Get(_ context.Context, tag string) (ReleaseState, error) {
	if _, ok := s.Tags[tag]; !ok {
		return ReleaseState{}, nil
	}
	return ReleaseState{Exists: true, Prerelease: s.Pre[tag]}, nil
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
	ID         int64 `json:"id"`
	Prerelease bool  `json:"prerelease"`
	Assets     []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"assets"`
}

// Get は tag のリリースの現状を返す(無ければ Exists=false)。読むだけなので token は要らない。
func (s *GitHubReleaseStore) Get(ctx context.Context, tag string) (ReleaseState, error) {
	rel, found, err := s.getRelease(ctx, tag)
	if err != nil || !found {
		return ReleaseState{}, err
	}
	return ReleaseState{Exists: true, Prerelease: rel.Prerelease}, nil
}

// Upload は tag のリリースを get-or-create し、各アセットを(同名は置換して)アップロードする。
// opt は**新しく作るときだけ**効く(既存のリリースの prerelease 状態は変えない)。
func (s *GitHubReleaseStore) Upload(ctx context.Context, tag, releaseName string, assets []ReleaseAsset, opt ReleaseOptions) error {
	if s.Token == "" {
		return fmt.Errorf("GITHUB_TOKEN required to upload the release to %s/%s", s.Owner, s.Repo)
	}
	rel, err := s.ensureRelease(ctx, tag, releaseName, opt)
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

// getRelease は tag のリリースを引く。無ければ found=false(エラーではない)。
func (s *GitHubReleaseStore) getRelease(ctx context.Context, tag string) (*ghRelease, bool, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", s.api(), s.Owner, s.Repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	s.auth(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		var rel ghRelease
		if err := json.Unmarshal(body, &rel); err != nil {
			return nil, false, err
		}
		return &rel, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("github get release %s: %s: %s", tag, resp.Status, snippet(body))
	}
}

// ensureRelease は tag のリリースを取得し、無ければ opt の属性で作る。
func (s *GitHubReleaseStore) ensureRelease(ctx context.Context, tag, name string, opt ReleaseOptions) (*ghRelease, error) {
	rel, found, err := s.getRelease(ctx, tag)
	if err != nil {
		return nil, err
	}
	if found {
		return rel, nil
	}
	return s.createRelease(ctx, tag, name, opt)
}

func (s *GitHubReleaseStore) createRelease(ctx context.Context, tag, name string, opt ReleaseOptions) (*ghRelease, error) {
	if name == "" {
		name = tag
	}
	payload := map[string]any{"tag_name": tag, "name": name}
	if opt.Prerelease {
		// GitHub は prerelease を latest として扱わない —— releases/latest/download/ は旧版を指したまま。
		payload["prerelease"] = true
	}
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
