package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// winget_probe.go — gated(winget)の実体照合(設計 04・11A)。記録した PR URL から GitHub の
// PR 状態を引き、申請の進行(pr_open/merged/closed)を実状態に更新する。マージはしない。

// WingetProbe は PR URL の状態を GitHub API で引く。
type WingetProbe struct {
	Token string // GITHUB_TOKEN(無くても公開 PR は読めるがレート制限あり)
	API   string // 既定 https://api.github.com(テストで差し替え)
	HTTP  *http.Client
}

var prURLRe = regexp.MustCompile(`github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

// ProbePR は PR URL から申請状態を返す: pr_open | merged | closed。
func (w *WingetProbe) ProbePR(ctx context.Context, prURL string) (string, error) {
	m := prURLRe.FindStringSubmatch(prURL)
	if m == nil {
		return "", fmt.Errorf("cannot parse PR url %q", prURL)
	}
	owner, repo, num := m[1], m[2], m[3]
	api := w.API
	if api == "" {
		api = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%s", api, owner, repo, num)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if w.Token != "" {
		req.Header.Set("Authorization", "Bearer "+w.Token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	cl := w.HTTP
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github pr %s: %s", url, resp.Status)
	}
	var pr struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", err
	}
	switch {
	case pr.State == "open":
		return "pr_open", nil
	case pr.Merged:
		return "merged", nil
	default:
		return "closed", nil
	}
}
