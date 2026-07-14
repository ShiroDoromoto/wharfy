package channel

// releases.go — GitHub Release に「配ったはずの資産」が実在するかを照合する(verify 用の読み手)。
//
// アップロードは 200 を返しても、資産の取りこぼし(publish の skip・部分失敗・手で消した)は
// 起きる。利用者はダウンロード時に 404 を踏むが、配布者は release が成功した記憶しか持たない。
//
// 照合の基準になる資産マニフェストは、wharfy 自身が書く latest.json(os/arch → ダウンロード URL)。
// GoReleaser 経路なら checksums マニフェストもあるので、在れば両方を合わせて期待集合にする。
// wharfy のネイティブ経路(BYO-binary)はこれを発行しないので、主にはできない(D-4)。
//
// 既定(Audit)はバイナリ本体を落とさない — 資産名の実在照合まで(D-4)。名前が在れば緑になるので、
// 途中で切れた・差し替えられた資産は捕まらない。それを捕まえるのが VerifyChecksums で、
// 資産を落として sha256 を検算する。数百 MB を毎回落とさないため verify --install でだけ呼ぶ。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// ManifestChecksums は GoReleaser の checksums マニフェストの素名。実際の資産名は既定の
	// name_template `{{ .ProjectName }}_{{ .Version }}_checksums.txt` が効いてプロジェクト名と版を
	// 含むので、固定名では実リリースに一度も当たらない(D-5)。拾うのは IsChecksumsManifest。
	ManifestChecksums = "checksums.txt"
)

// IsChecksumsManifest は Release 資産の名前が checksums マニフェストかを判定する。
// 既定の name_template が付ける `<project>_<version>_` 前置と、name_template を素名に潰した
// 配布者の両方を拾う。
func IsChecksumsManifest(name string) bool {
	return name == ManifestChecksums || strings.HasSuffix(name, "_"+ManifestChecksums)
}

// ReleasesProbe は tag のリリースからアセット名一覧とマニフェストを読む Probe 専用の型。
type ReleasesProbe struct {
	Owner, Repo string
	Token       string // private repo の Release メタデータ取得に要る(資産本体は落とさない)
	API         string // 既定 https://api.github.com
	HTTP        *http.Client
}

// ReleaseAudit は 1 つの Release に対する実在照合の結果。
type ReleaseAudit struct {
	Found bool // その tag の Release が在るか
	// Prerelease は、その Release が prerelease である(資産は在るが GitHub の latest ではない)。
	// 資産としては何も欠けていないので検証は普通に通る —— しかし利用者はまだこの版を受け取って
	// いない。verify が「確かめた物」と「配ってある物」を取り違えないための一点。
	Prerelease bool
	Manifests  []string // 照合に使えたマニフェストの資産名
	Version    string   // latest.json が名乗る版(checksums しか無ければ空)
	Expected   []string // マニフェストが載せる資産名(昇順)
	Missing    []string // そのうち Release に実在しないもの(昇順)

	// HasLatestJSON は latest.json が Release に実在したか。release は github(owner/repo)が
	// 解決できる限り必ずこれを上げるので、無い Release は壊れている(更新チェックの向き先が 404)。
	// Version は latest.json が版を名乗らなければ空になるため、有無の判定には使えない。
	HasLatestJSON bool

	// Checksums は checksums マニフェスト由来の 資産名 → 期待 sha256。sha を持つのはこの
	// マニフェストだけなので、latest.json しか無い Release では空になる(検算できない)。
	Checksums map[string]string
	// URLs は 資産名 → ダウンロード URL(Release に実在するものだけ)。VerifyChecksums が使う。
	URLs map[string]string
	// Digests は 資産名 → sha256(GitHub が**実際に配っているバイト列**から出したもの・16 進小文字)。
	//
	// 配布者が書いた checksums マニフェストとは出どころが違う: あれは「こういう物を上げたと宣言した」
	// 記述で、こちらは「いま落ちてくる物はこれ」という観測。だから来歴(attest)の照合はこちらで引く
	// ——落とさずに、利用者が受け取るバイト列そのものの digest を見られる。
	// 古い GitHub は digest を返さないので、その場合は空になる。
	Digests map[string]string
}

// ChecksumMismatch は落とした資産の sha256 が checksums マニフェストと食い違ったこと。
type ChecksumMismatch struct {
	Asset string
	Want  string // マニフェストが載せる sha256
	Got   string // 実際に落ちてきた資産の sha256
}

func (m ChecksumMismatch) String() string {
	return m.Asset + ": manifest says " + m.Want + ", the asset hashes to " + m.Got
}

type ghReleaseAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	// Digest は GitHub が資産のバイト列から出した digest("sha256:<hex>")。来歴の照合に使う。
	Digest string `json:"digest"`
}

type ghReleaseAssets struct {
	Prerelease bool             `json:"prerelease"`
	Assets     []ghReleaseAsset `json:"assets"`
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

// Latest は「いま配ってある最新版」を Release 側から引く(tag_name の v を落とした版)。
//
// verify の基点。ローカルの記録(.wharfy/state.json)は生成物ゆえ gitignore され、別ジョブにも
// まっさらな clone にも持ち越されない。記録が無いからといって「確かめられません」と返していたら、
// 配った後に「今も入るか」を確かめる手段が無くなる —— 実体を見に行けばよい。
// Release が 1 つも無ければ found=false(エラーではない)。
func (p *ReleasesProbe) Latest(ctx context.Context) (version string, found bool, err error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", p.api(), p.Owner, p.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.client().Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		var rel struct {
			TagName string `json:"tag_name"`
		}
		if err := json.Unmarshal(body, &rel); err != nil {
			return "", false, err
		}
		tag := strings.TrimSpace(rel.TagName)
		if tag == "" {
			return "", false, nil
		}
		return strings.TrimPrefix(tag, "v"), true, nil
	case http.StatusNotFound:
		return "", false, nil // Release がまだ 1 つも無い
	default:
		return "", false, fmt.Errorf("github get latest release: %s: %s", resp.Status, snippet(body))
	}
}

// Audit は v<version> の Release を引き、マニフェストが載せる資産がすべて実在するかを確かめる。
// Release ごと不在なら Found=false(エラーではない)。マニフェストが無ければ Manifests が空。
func (p *ReleasesProbe) Audit(ctx context.Context, version string) (ReleaseAudit, error) {
	rel, found, err := p.releaseByTag(ctx, "v"+version)
	if err != nil || !found {
		return ReleaseAudit{}, err
	}

	present := map[string]string{} // 資産名 → ダウンロード URL
	digests := map[string]string{} // 資産名 → GitHub が配るバイト列の sha256
	for _, a := range rel.Assets {
		present[a.Name] = a.DownloadURL
		if h, ok := strings.CutPrefix(a.Digest, "sha256:"); ok {
			digests[a.Name] = strings.ToLower(h)
		}
	}
	audit := ReleaseAudit{Found: true, Prerelease: rel.Prerelease, URLs: present, Digests: digests}

	expected := map[string]bool{}
	if url, ok := present[ManifestLatestJSON]; ok {
		doc, err := p.fetchLatestJSON(ctx, url)
		if err != nil {
			return ReleaseAudit{}, err
		}
		audit.HasLatestJSON = true
		audit.Manifests = append(audit.Manifests, ManifestLatestJSON)
		audit.Version = doc.Version
		for _, u := range doc.Assets {
			expected[path.Base(u)] = true
		}
	}
	for _, name := range checksumsAssets(present) {
		sums, err := p.fetchChecksums(ctx, present[name])
		if err != nil {
			return ReleaseAudit{}, err
		}
		audit.Manifests = append(audit.Manifests, name)
		if audit.Checksums == nil {
			audit.Checksums = map[string]string{}
		}
		for n, sum := range sums {
			expected[n] = true
			audit.Checksums[n] = sum
		}
	}
	// マニフェスト自身は自分を載せない。載っていても欠損には数えない。
	delete(expected, ManifestLatestJSON)
	for _, m := range audit.Manifests {
		delete(expected, m)
	}

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

// checksumsAssets は Release に上がっている checksums マニフェストの資産名を昇順で返す。
// 名前が版を含むので呼び手が当てにいけない — 資産一覧の側から拾う。
func checksumsAssets(present map[string]string) []string {
	var names []string
	for name := range present {
		if IsChecksumsManifest(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
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

// fetchChecksums は checksums.txt の各行を 資産名 → sha256 に読む(GoReleaser 形式 "<sha>  <name>"、
// binary mode の "*name" も剥がす)。名前は Audit の期待集合に、sha は VerifyChecksums の検算に使う。
func (p *ReleasesProbe) fetchChecksums(ctx context.Context, url string) (map[string]string, error) {
	body, err := p.fetchAsset(ctx, url)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sums[strings.TrimPrefix(fields[len(fields)-1], "*")] = fields[0]
	}
	return sums, nil
}

// VerifyChecksums は checksums マニフェストが載せる資産を実際に落とし、sha256 を検算する。
// 食い違ったものを昇順で返す(空なら全一致)。
//
// 名前の実在照合(Audit)では「途中で切れた・後から差し替えられた資産」を捕まえられない
// ——名前は在るからだ。それを捕まえられる唯一の手が、本体を落として突き合わせることになる。
// Release に実在しない資産は Audit が Missing として既に報告しているので、ここでは飛ばす。
//
// 資産は sha256 に流し込むだけでメモリには溜めない。呼び手が ctx で上限を掛ける。
func (p *ReleasesProbe) VerifyChecksums(ctx context.Context, audit ReleaseAudit) ([]ChecksumMismatch, error) {
	names := make([]string, 0, len(audit.Checksums))
	for name := range audit.Checksums {
		if _, ok := audit.URLs[name]; ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var bad []ChecksumMismatch
	for _, name := range names {
		got, err := p.assetSHA256(ctx, audit.URLs[name])
		if err != nil {
			return nil, err
		}
		if want := audit.Checksums[name]; !strings.EqualFold(got, want) {
			bad = append(bad, ChecksumMismatch{Asset: name, Want: want, Got: got})
		}
	}
	return bad, nil
}

// assetSHA256 は資産を落としながら sha256 を計算する。
// 取得は browser_download_url を素で叩く — 認証を足さないのは、利用者が踏むのと同じ経路を見るため。
func (p *ReleasesProbe) assetSHA256(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("fetch %s: %s: %s", url, resp.Status, snippet(body))
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", fmt.Errorf("read %s: %w", url, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
