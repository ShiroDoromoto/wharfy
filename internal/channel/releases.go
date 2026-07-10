package channel

// releases.go — GitHub Release に「配ったはずの資産」が実在するかを照合する(verify 用の読み手)。
//
// アップロードは 200 を返しても、資産の取りこぼし(publish の skip・部分失敗・手で消した)は
// 起きる。利用者はダウンロード時に 404 を踏むが、配布者は release が成功した記憶しか持たない。
//
// 照合の基準になる資産マニフェストは、wharfy 自身が書く latest.json(os/arch → ダウンロード URL)。
// GoReleaser 経路なら checksums.txt もあるので、在れば両方を合わせて期待集合にする。
// wharfy のネイティブ経路(BYO-binary)は checksums.txt を発行しないので、これを主にはできない(D-4)。
//
// バイナリ本体は落とさない — 資産名の実在照合まで(D-4)。sha256 の検算は verify --install の範囲。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
)

// 資産マニフェストの名前(Release アセットとして上がっている)。
const (
	ManifestLatestJSON = "latest.json"
	ManifestChecksums  = "checksums.txt"
)

// ReleasesProbe は tag のリリースからアセット名一覧とマニフェストを読む Probe 専用の型。
type ReleasesProbe struct {
	Owner, Repo string
	Token       string // private repo の Release メタデータ取得に要る(資産本体は落とさない)
	API         string // 既定 https://api.github.com
	HTTP        *http.Client
}

// ReleaseAudit は 1 つの Release に対する実在照合の結果。
type ReleaseAudit struct {
	Found     bool     // その tag の Release が在るか
	Manifests []string // 照合に使えたマニフェスト(空なら照合不能 = skip)
	Version   string   // latest.json が名乗る版(checksums.txt しか無ければ空)
	Expected  []string // マニフェストが載せる資産名(昇順)
	Missing   []string // そのうち Release に実在しないもの(昇順)
}

type ghReleaseAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

type ghReleaseAssets struct {
	Assets []ghReleaseAsset `json:"assets"`
}

// latestJSONDoc は latest.json のうち照合に要る部分(契約は schemas/latest.json)。
type latestJSONDoc struct {
	Version string            `json:"version"`
	Assets  map[string]string `json:"assets"`
}

func (p *ReleasesProbe) client() *http.Client {
	if p.HTTP == nil {
		return http.DefaultClient
	}
	return p.HTTP
}

func (p *ReleasesProbe) api() string {
	if p.API == "" {
		return "https://api.github.com"
	}
	return p.API
}

// Audit は v<version> の Release を引き、マニフェストが載せる資産がすべて実在するかを確かめる。
// Release ごと不在なら Found=false(エラーではない)。マニフェストが無ければ Manifests が空。
func (p *ReleasesProbe) Audit(ctx context.Context, version string) (ReleaseAudit, error) {
	rel, found, err := p.releaseByTag(ctx, "v"+version)
	if err != nil || !found {
		return ReleaseAudit{}, err
	}

	present := map[string]string{} // 資産名 → ダウンロード URL
	for _, a := range rel.Assets {
		present[a.Name] = a.DownloadURL
	}
	audit := ReleaseAudit{Found: true}

	expected := map[string]bool{}
	if url, ok := present[ManifestLatestJSON]; ok {
		doc, err := p.fetchLatestJSON(ctx, url)
		if err != nil {
			return ReleaseAudit{}, err
		}
		audit.Manifests = append(audit.Manifests, ManifestLatestJSON)
		audit.Version = doc.Version
		for _, u := range doc.Assets {
			expected[path.Base(u)] = true
		}
	}
	if url, ok := present[ManifestChecksums]; ok {
		names, err := p.fetchChecksumNames(ctx, url)
		if err != nil {
			return ReleaseAudit{}, err
		}
		audit.Manifests = append(audit.Manifests, ManifestChecksums)
		for _, n := range names {
			expected[n] = true
		}
	}
	// マニフェスト自身は自分を載せない。載っていても欠損には数えない。
	delete(expected, ManifestLatestJSON)
	delete(expected, ManifestChecksums)

	for name := range expected {
		audit.Expected = append(audit.Expected, name)
		if _, ok := present[name]; !ok {
			audit.Missing = append(audit.Missing, name)
		}
	}
	sort.Strings(audit.Expected)
	sort.Strings(audit.Missing)
	return audit, nil
}

// releaseByTag は tag の Release を引く(404 は found=false・エラーではない)。
func (p *ReleasesProbe) releaseByTag(ctx context.Context, tag string) (ghReleaseAssets, bool, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", p.api(), p.Owner, p.Repo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ghReleaseAssets{}, false, err
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.client().Do(req)
	if err != nil {
		return ghReleaseAssets{}, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		var rel ghReleaseAssets
		if err := json.Unmarshal(body, &rel); err != nil {
			return ghReleaseAssets{}, false, err
		}
		return rel, true, nil
	case http.StatusNotFound:
		return ghReleaseAssets{}, false, nil
	default:
		return ghReleaseAssets{}, false, fmt.Errorf("github get release %s: %s: %s", tag, resp.Status, snippet(body))
	}
}

// fetchLatestJSON は Release に載る latest.json を読む。
// 取得は browser_download_url を素で叩く — 認証を足さないのは、利用者が踏むのと同じ経路を見るため。
func (p *ReleasesProbe) fetchLatestJSON(ctx context.Context, url string) (latestJSONDoc, error) {
	body, err := p.fetchAsset(ctx, url)
	if err != nil {
		return latestJSONDoc{}, err
	}
	var doc latestJSONDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return latestJSONDoc{}, fmt.Errorf("parse %s: %w", ManifestLatestJSON, err)
	}
	return doc, nil
}

// fetchChecksumNames は checksums.txt の各行から資産名を読む(GoReleaser 形式 "<sha>  <name>"、
// binary mode の "*name" も剥がす)。sha は使わない — 検算は資産本体を落とす verify --install の範囲。
func (p *ReleasesProbe) fetchChecksumNames(ctx context.Context, url string) ([]string, error) {
	body, err := p.fetchAsset(ctx, url)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		names = append(names, strings.TrimPrefix(fields[len(fields)-1], "*"))
	}
	return names, nil
}

func (p *ReleasesProbe) fetchAsset(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s: %s", url, resp.Status, snippet(body))
	}
	return body, nil
}
