package channel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// releaseServer は tag の Release とそのアセット本体を返す最小の GitHub API。
// assets は「Release に実在する」アセット(名前 → 中身)。
func releaseServer(t *testing.T, tag string, assets map[string]string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/demo/releases/tags/"+tag:
			list := make([]map[string]string, 0, len(assets))
			for name := range assets {
				list = append(list, map[string]string{
					"name":                 name,
					"browser_download_url": srv.URL + "/dl/" + name,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"assets": list})
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			body, ok := assets[strings.TrimPrefix(r.URL.Path, "/dl/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func latestJSONBody(version string, names ...string) string {
	assets := map[string]string{}
	for i, n := range names {
		assets["key"+string(rune('a'+i))] = "https://github.com/acme/demo/releases/download/v" + version + "/" + n
	}
	b, _ := json.Marshal(map[string]any{"version": version, "assets": assets})
	return string(b)
}

func auditOf(t *testing.T, srv *httptest.Server, version string) ReleaseAudit {
	t.Helper()
	p := &ReleasesProbe{Owner: "acme", Repo: "demo", API: srv.URL}
	audit, err := p.Audit(context.Background(), version)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	return audit
}

// latest.json が載せる資産がすべて実在する → 欠損なし。
func TestReleasesAuditAllAssetsPresent(t *testing.T) {
	srv := releaseServer(t, "v1.2.0", map[string]string{
		"latest.json":                   latestJSONBody("1.2.0", "demo_1.2.0_linux_amd64.tar.gz"),
		"demo_1.2.0_linux_amd64.tar.gz": "binary",
	})
	audit := auditOf(t, srv, "1.2.0")
	if !audit.Found || audit.Version != "1.2.0" {
		t.Fatalf("release should be found with its manifest version: %+v", audit)
	}
	if len(audit.Missing) != 0 {
		t.Errorf("nothing should be missing: %+v", audit.Missing)
	}
	if !reflect.DeepEqual(audit.Manifests, []string{ManifestLatestJSON}) {
		t.Errorf("manifests = %v", audit.Manifests)
	}
	// マニフェスト自身は期待集合に数えない(自分を載せていないので欠損に見える)。
	if !reflect.DeepEqual(audit.Expected, []string{"demo_1.2.0_linux_amd64.tar.gz"}) {
		t.Errorf("expected = %v", audit.Expected)
	}
}

// latest.json に載るのに Release に上がっていない資産 → 欠損。利用者は 404 を踏む。
func TestReleasesAuditReportsMissingAssets(t *testing.T) {
	srv := releaseServer(t, "v1.2.0", map[string]string{
		"latest.json":                   latestJSONBody("1.2.0", "demo_1.2.0_linux_amd64.tar.gz", "demo_1.2.0_windows_amd64.zip"),
		"demo_1.2.0_linux_amd64.tar.gz": "binary",
	})
	audit := auditOf(t, srv, "1.2.0")
	if !reflect.DeepEqual(audit.Missing, []string{"demo_1.2.0_windows_amd64.zip"}) {
		t.Errorf("missing = %v", audit.Missing)
	}
}

// GoReleaser の checksums も期待集合に足す。latest.json と両方あれば和集合。
// 資産名は既定の name_template が効いて <project>_<version>_checksums.txt になる(D-5)ので、
// フィクスチャも実物と同じ名前にする — 素名では実リリースに一度も当たらない。
func TestReleasesAuditUnionsChecksums(t *testing.T) {
	srv := releaseServer(t, "v1.2.0", map[string]string{
		"latest.json":              latestJSONBody("1.2.0", "demo_linux.tar.gz"),
		"demo_1.2.0_checksums.txt": "abc123  demo_linux.tar.gz\ndef456 *demo_windows.zip\n",
		"demo_linux.tar.gz":        "binary",
	})
	audit := auditOf(t, srv, "1.2.0")
	if !reflect.DeepEqual(audit.Manifests, []string{ManifestLatestJSON, "demo_1.2.0_checksums.txt"}) {
		t.Fatalf("both manifests should be used: %v", audit.Manifests)
	}
	if !reflect.DeepEqual(audit.Expected, []string{"demo_linux.tar.gz", "demo_windows.zip"}) {
		t.Errorf("expected should union both manifests: %v", audit.Expected)
	}
	if !reflect.DeepEqual(audit.Missing, []string{"demo_windows.zip"}) {
		t.Errorf("missing = %v", audit.Missing)
	}
}

// name_template を素の checksums.txt に潰した配布者も拾う。マニフェスト自身は欠損に数えない。
func TestReleasesAuditBareChecksumsName(t *testing.T) {
	srv := releaseServer(t, "v1.2.0", map[string]string{
		"checksums.txt":     "abc123  demo_linux.tar.gz\n",
		"demo_linux.tar.gz": "binary",
	})
	audit := auditOf(t, srv, "1.2.0")
	if !reflect.DeepEqual(audit.Manifests, []string{ManifestChecksums}) {
		t.Fatalf("bare checksums.txt should be used as a manifest: %v", audit.Manifests)
	}
	if len(audit.Missing) != 0 {
		t.Errorf("nothing should be missing: %v", audit.Missing)
	}
}

// checksums マニフェストが自分自身を載せていても期待集合に数えない。
// 実在するので Missing には出ず、Expected の数だけが静かに狂う — 名前が版を含むぶん、
// 固定名で引く delete では取りこぼす。
func TestReleasesAuditExcludesChecksumsItselfFromExpected(t *testing.T) {
	srv := releaseServer(t, "v1.2.0", map[string]string{
		"demo_1.2.0_checksums.txt": "abc123  demo_linux.tar.gz\ndef456  demo_1.2.0_checksums.txt\n",
		"demo_linux.tar.gz":        "binary",
	})
	audit := auditOf(t, srv, "1.2.0")
	if !reflect.DeepEqual(audit.Expected, []string{"demo_linux.tar.gz"}) {
		t.Errorf("the manifest must not count itself: %v", audit.Expected)
	}
}

// checksums で終わらない資産をマニフェストと取り違えない(demochecksums.txt は別物)。
func TestReleasesAuditIgnoresLookalikeAssets(t *testing.T) {
	srv := releaseServer(t, "v1.2.0", map[string]string{
		"latest.json":           latestJSONBody("1.2.0", "demo_linux.tar.gz"),
		"demochecksums.txt":     "not a manifest",
		"demo_linux.tar.gz":     "binary",
		"checksums.txt.minisig": "signature",
	})
	audit := auditOf(t, srv, "1.2.0")
	if !reflect.DeepEqual(audit.Manifests, []string{ManifestLatestJSON}) {
		t.Errorf("only latest.json is a manifest here: %v", audit.Manifests)
	}
}

// マニフェストの無い旧リリースは「照合できない」。資産が在るように見えても期待集合が無い。
func TestReleasesAuditWithoutManifest(t *testing.T) {
	srv := releaseServer(t, "v0.9.0", map[string]string{"demo_linux.tar.gz": "binary"})
	audit := auditOf(t, srv, "0.9.0")
	if !audit.Found {
		t.Fatalf("the release itself exists: %+v", audit)
	}
	if len(audit.Manifests) != 0 || len(audit.Expected) != 0 {
		t.Errorf("without a manifest nothing can be expected: %+v", audit)
	}
}

// tag ごと不在 → Found=false(エラーではない。呼び手が verify_failed にする)。
func TestReleasesAuditReleaseNotFound(t *testing.T) {
	srv := releaseServer(t, "v1.2.0", map[string]string{})
	p := &ReleasesProbe{Owner: "acme", Repo: "demo", API: srv.URL}
	audit, err := p.Audit(context.Background(), "9.9.9")
	if err != nil {
		t.Fatalf("a missing release is not a probe error: %v", err)
	}
	if audit.Found {
		t.Errorf("audit = %+v", audit)
	}
}

// checksumsBody は assets の本文から GoReleaser 形式の checksums マニフェストを組む。
func checksumsBody(assets map[string]string, names ...string) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256([]byte(assets[n])), n)
	}
	return b.String()
}

// 落とした資産の sha256 が checksums と一致する → 食い違いなし。
func TestVerifyChecksumsAllMatch(t *testing.T) {
	assets := map[string]string{"demo_linux.tar.gz": "bin", "demo_windows.zip": "exe"}
	assets["demo_1.2.0_checksums.txt"] = checksumsBody(assets, "demo_linux.tar.gz", "demo_windows.zip")
	srv := releaseServer(t, "v1.2.0", assets)

	p := &ReleasesProbe{Owner: "acme", Repo: "demo", API: srv.URL}
	bad, err := p.VerifyChecksums(context.Background(), auditOf(t, srv, "1.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Errorf("assets that hash to what the manifest says must not be reported: %+v", bad)
	}
}

// マニフェストを書いた後に中身が変わった資産は食い違いとして返る。名前は在るので Audit では
// 捕まらない ——検算だけが捕まえられる壊れ方。
func TestVerifyChecksumsReportsMismatch(t *testing.T) {
	assets := map[string]string{"demo_linux.tar.gz": "bin", "demo_windows.zip": "exe"}
	assets["demo_1.2.0_checksums.txt"] = checksumsBody(assets, "demo_linux.tar.gz", "demo_windows.zip")
	assets["demo_windows.zip"] = "tampered"
	srv := releaseServer(t, "v1.2.0", assets)

	audit := auditOf(t, srv, "1.2.0")
	if len(audit.Missing) != 0 {
		t.Fatalf("the tampered asset is still present by name: %+v", audit)
	}
	p := &ReleasesProbe{Owner: "acme", Repo: "demo", API: srv.URL}
	bad, err := p.VerifyChecksums(context.Background(), audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || bad[0].Asset != "demo_windows.zip" {
		t.Fatalf("only the tampered asset should be reported: %+v", bad)
	}
	if bad[0].Want == bad[0].Got || bad[0].Want == "" || bad[0].Got == "" {
		t.Errorf("the mismatch must carry both sha256: %+v", bad[0])
	}
}

// checksums に載っていても Release に実在しない資産は飛ばす(Audit が Missing で報告済み)。
// 落としにいけば 404 で probe ごと落ちるので、欠損を検算の失敗にすり替えないため。
func TestVerifyChecksumsSkipsMissingAssets(t *testing.T) {
	assets := map[string]string{"demo_linux.tar.gz": "bin"}
	assets["demo_1.2.0_checksums.txt"] = checksumsBody(
		map[string]string{"demo_linux.tar.gz": "bin", "demo_windows.zip": "exe"},
		"demo_linux.tar.gz", "demo_windows.zip")
	srv := releaseServer(t, "v1.2.0", assets)

	audit := auditOf(t, srv, "1.2.0")
	if len(audit.Missing) != 1 || audit.Missing[0] != "demo_windows.zip" {
		t.Fatalf("the absent asset should be Missing: %+v", audit)
	}
	p := &ReleasesProbe{Owner: "acme", Repo: "demo", API: srv.URL}
	bad, err := p.VerifyChecksums(context.Background(), audit)
	if err != nil {
		t.Fatalf("a missing asset must not turn into a probe error: %v", err)
	}
	if len(bad) != 0 {
		t.Errorf("the present asset matches; the absent one is not a mismatch: %+v", bad)
	}
}

// latest.json しか無い Release は sha を持たない → 検算する対象がゼロ(呼び手が partial にする)。
func TestVerifyChecksumsWithoutChecksumsManifest(t *testing.T) {
	srv := releaseServer(t, "v1.2.0", map[string]string{
		ManifestLatestJSON:  latestJSONBody("1.2.0", "demo_linux.tar.gz"),
		"demo_linux.tar.gz": "bin",
	})
	audit := auditOf(t, srv, "1.2.0")
	if len(audit.Checksums) != 0 {
		t.Fatalf("latest.json carries no sha256: %+v", audit.Checksums)
	}
	p := &ReleasesProbe{Owner: "acme", Repo: "demo", API: srv.URL}
	bad, err := p.VerifyChecksums(context.Background(), audit)
	if err != nil || len(bad) != 0 {
		t.Errorf("nothing to compare = no mismatch, no error: bad=%+v err=%v", bad, err)
	}
}

// Latest は Release 側から「いま配ってある最新版」を引く(verify がローカルの記録なしで動く基点)。
func TestReleasesProbeLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/demo/releases/latest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.4.0"})
	}))
	defer srv.Close()

	p := &ReleasesProbe{Owner: "acme", Repo: "demo", API: srv.URL}
	v, found, err := p.Latest(context.Background())
	if err != nil || !found || v != "1.4.0" {
		t.Fatalf("latest release should come back without the v: %q %v %v", v, found, err)
	}
}

// Release が 1 つも無いのはエラーではない(found=false)。verify はそこで次の基点へ落ちる。
func TestReleasesProbeLatestNoRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := &ReleasesProbe{Owner: "acme", Repo: "demo", API: srv.URL}
	if _, found, err := p.Latest(context.Background()); err != nil || found {
		t.Fatalf("no release at all must not be an error: found=%v err=%v", found, err)
	}
}
