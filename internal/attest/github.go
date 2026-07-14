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
	"net/url"
	"strings"

	"github.com/golang/snappy"
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

	resp, err := s.client().Do(req)
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

// bundleBlobLimit は bundle 1 つの読み取り上限。証明は数十 KB で、これを超えるものは
// 預け先の異常——検算する側が無制限に読まされないようにする。
const bundleBlobLimit = 8 << 20

// Bundles は digest に預けてある来歴(provenance の bundle)を返す。証明が無ければ空(エラーではない)。
//
// 応答の形に 2 つ癖がある。どちらも GitHub の保管の都合で、証明そのものの性質ではない:
//   - bundle は本文に載ることも、載らずに bundle_url(署名付きの blob URL)だけのこともある。
//   - その blob は snappy で縮めてある(Content-Type: application/x-snappy)。
//
// 引くのは provenance だけ(predicate_type)。同じ digest には別種の証言(SBOM 等)も預けられるので、
// 絞らないと「来歴ではない証明」を来歴として数えることになる。
func (s *GitHubStore) Bundles(ctx context.Context, sha256 string) ([][]byte, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/attestations/sha256:%s?predicate_type=%s",
		s.api(), s.Owner, s.Repo, sha256, url.QueryEscape(predicateType))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("attest: fetch attestations: %w", err)
	}
	defer resp.Body.Close()
	// 404 は「その digest には何も預けられていない」。証明が無いことは検算の失敗ではなく観測結果なので、
	// エラーにはせず空で返す(呼び手が「まだ付けていない」として語る)。
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("attest: fetch attestations: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var doc struct {
		Attestations []struct {
			Bundle    json.RawMessage `json:"bundle"`
			BundleURL string          `json:"bundle_url"`
		} `json:"attestations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("attest: decode attestations: %w", err)
	}

	var out [][]byte
	for _, a := range doc.Attestations {
		if len(a.Bundle) > 0 && string(a.Bundle) != "null" {
			out = append(out, a.Bundle)
			continue
		}
		if a.BundleURL == "" {
			continue
		}
		b, err := s.fetchBundleBlob(ctx, a.BundleURL)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// fetchBundleBlob は bundle_url の blob を取って bundle の JSON にする。
//
// URL 自体が署名付き(SAS)で、預け先も GitHub ではない。だから Authorization は**付けない**——
// リポジトリのトークンを他所のホストへ渡さない。
func (s *GitHubStore) fetchBundleBlob(ctx context.Context, blobURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("attest: fetch attestation bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("attest: fetch attestation bundle: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, bundleBlobLimit))
	if err != nil {
		return nil, fmt.Errorf("attest: read attestation bundle: %w", err)
	}
	// 縮めてあれば戻す。縮めずに置く日が来ても読めるよう、JSON ならそのまま通す
	// (Content-Type ではなく中身で決める——保管の都合が変わっても検算が止まらない)。
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return raw, nil
	}
	b, err := snappy.Decode(nil, raw)
	if err != nil {
		return nil, fmt.Errorf("attest: decompress attestation bundle: %w", err)
	}
	return b, nil
}

func (s *GitHubStore) client() *http.Client {
	if s.HTTP == nil {
		return http.DefaultClient
	}
	return s.HTTP
}
