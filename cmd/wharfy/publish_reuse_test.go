package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
)

// ghArtifactsServer は tag の Release が artifacts.json を持っている状態を返す最小の GitHub API。
func ghArtifactsServer(t *testing.T, tag string, set build.ArtifactSet) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/app/releases/tags/"+tag:
			_ = json.NewEncoder(w).Encode(map[string]any{"assets": []map[string]string{{
				"name":                 channel.ManifestArtifacts,
				"browser_download_url": srv.URL + "/dl/" + channel.ManifestArtifacts,
			}}})
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPublishReusesTheReleaseAcrossJobs: release と publish が別の run で走っても、publish は
// 資産を上げ直さない —— Release 自身が持つ artifacts.json を読む。
//
// これが無いと、手元に記録が無い runner では publish が release をやり直し、**検証したのとは別の
// バイト列**に貼り替わる(来歴も外れる)。prerelease → 検証 → promote → publish の動線は、
// publish が別ジョブで走るのが前提なので、ここが崩れると窓を開けた意味そのものが消える。
func TestPublishReusesTheReleaseAcrossJobs(t *testing.T) {
	root := scratchPrebuilt(t) // 記録(.wharfy/artifacts.json)は無い = 別ジョブの runner
	tagScratch(t, root, "v0.1.0")
	chdir(t, root)
	t.Setenv("GITHUB_TOKEN", "tok")

	set := build.ArtifactSet{Version: "0.1.0", Artifacts: []build.Artifact{
		{OS: "darwin", Arch: "arm64", Path: "dist/app_0.1.0_darwin_arm64.tar.gz", SHA256: "aa"},
		{OS: "linux", Arch: "amd64", Path: "dist/app_0.1.0_linux_amd64.tar.gz", SHA256: "bb"},
	}}
	srv := ghArtifactsServer(t, "v0.1.0", set)
	old := newReleasesProbe
	newReleasesProbe = func(owner, repo string) *channel.ReleasesProbe {
		return &channel.ReleasesProbe{Owner: owner, Repo: repo, API: srv.URL}
	}
	defer func() { newReleasesProbe = old }()

	store := channel.NewInMemoryReleaseStore()
	defer swapReleaseStore(store)()
	tap := channel.NewInMemoryTapStore()
	defer swapTapStore(tap)()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"homebrew"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if store.Uploads != 0 {
		t.Errorf("publish must not re-upload the assets it is about to point at (uploads=%d)", store.Uploads)
	}
	if tap.Commits == 0 {
		t.Fatal("the formula should have been written from the release's own record")
	}
	// formula は Release が持つ実 sha を引く(作り直した別のバイト列の sha ではない)。
	var formula string
	for _, f := range tap.Files {
		formula = f
	}
	if !strings.Contains(formula, "aa") || !strings.Contains(formula, "bb") {
		t.Errorf("the formula must carry the checksums recorded on the release:\n%s", formula)
	}
	// 引けた記録は手元にも書き戻る(後続の publish <ch> が同じ物を再利用できる)。
	if _, err := os.Stat(filepath.Join(root, build.ArtifactsFile)); err != nil {
		t.Errorf("the fetched record should be written back locally: %v", err)
	}
}
