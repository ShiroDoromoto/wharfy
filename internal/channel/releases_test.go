package channel

import (
	"context"
	"encoding/json"
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

// checksums.txt(GoReleaser 経路)も期待集合に足す。latest.json と両方あれば和集合。
func TestReleasesAuditUnionsChecksums(t *testing.T) {
	srv := releaseServer(t, "v1.2.0", map[string]string{
		"latest.json":       latestJSONBody("1.2.0", "demo_linux.tar.gz"),
		"checksums.txt":     "abc123  demo_linux.tar.gz\ndef456 *demo_windows.zip\n",
		"demo_linux.tar.gz": "binary",
	})
	audit := auditOf(t, srv, "1.2.0")
	if !reflect.DeepEqual(audit.Manifests, []string{ManifestLatestJSON, ManifestChecksums}) {
		t.Fatalf("both manifests should be used: %v", audit.Manifests)
	}
	if !reflect.DeepEqual(audit.Expected, []string{"demo_linux.tar.gz", "demo_windows.zip"}) {
		t.Errorf("expected should union both manifests: %v", audit.Expected)
	}
	if !reflect.DeepEqual(audit.Missing, []string{"demo_windows.zip"}) {
		t.Errorf("missing = %v", audit.Missing)
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
