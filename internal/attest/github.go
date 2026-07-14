package attest

// github.go — 署名済み bundle を GitHub の attestations API に預ける。
//
// actions/attest-build-provenance も、突き詰めればこの API を呼ぶ薄い Action でしかない。
// 預けた証明は subject の digest で引ける(GET /repos/{owner}/{repo}/attestations/{digest})ので、
// `gh attestation verify` はファイルさえあれば検算できる。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GitHubStore は attestations API への預け先。他の GitHub クライアント(channel の各 store)と
// 同じ形にそろえる: 既定 API は api.github.com、HTTP はテストで差し替える。
type GitHubStore struct {
	Owner, Repo string
	Token       string
	API         string // 既定 https://api.github.com
	HTTP        *http.Client
}

// NewGitHubStore は attestations API の預け先を作る。
func NewGitHubStore(owner, repo, token string) *GitHubStore {
	return &GitHubStore{Owner: owner, Repo: repo, Token: token, API: "https://api.github.com", HTTP: http.DefaultClient}
}

func (s *GitHubStore) api() string {
	if s.API == "" {
		return "https://api.github.com"
	}
	return strings.TrimSuffix(s.API, "/")
}

// Put は bundle を預け、GitHub が採番した attestation の id を返す。
func (s *GitHubStore) Put(ctx context.Context, bundleJSON []byte) (int64, error) {
	body, err := json.Marshal(map[string]json.RawMessage{"bundle": bundleJSON})
	if err != nil {
		return 0, err
	}
	url := fmt.Sprintf("%s/repos/%s/%s/attestations", s.api(), s.Owner, s.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	hc := s.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("attest: store attestation: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// 403 はまず permissions の欠落。原因を推測させず、そのまま次の一手を書く。
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		hint := ""
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			hint = " — the workflow needs permissions: attestations: write (and id-token: write)"
		}
		return 0, fmt.Errorf("attest: store attestation: %s: %s%s", resp.Status, strings.TrimSpace(string(msg)), hint)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("attest: decode attestation response: %w", err)
	}
	return out.ID, nil
}
