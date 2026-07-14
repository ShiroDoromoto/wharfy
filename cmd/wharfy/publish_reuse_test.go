package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/output"
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

// ghPackageServer は tag の Release が artifacts.json と deb 本体を持っている状態を返す GitHub API。
// deb の中身は debBytes をそのまま返す(すり替えを試すときは別のバイト列を渡す)。
func ghPackageServer(t *testing.T, tag, debName string, set build.ArtifactSet, debBytes []byte) *httptest.Server {
	t.Helper()
	manifest, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/app/releases/tags/"+tag:
			_ = json.NewEncoder(w).Encode(map[string]any{"assets": []map[string]string{
				{"name": channel.ManifestArtifacts, "browser_download_url": srv.URL + "/dl/" + channel.ManifestArtifacts},
				{"name": debName, "browser_download_url": srv.URL + "/dl/" + debName},
			}})
		case r.URL.Path == "/dl/"+channel.ManifestArtifacts:
			_, _ = w.Write(manifest)
		case r.URL.Path == "/dl/"+debName:
			_, _ = w.Write(debBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// aptFromRelease は「dist を持たない別ジョブ(promote 後の publish)で apt を配る」場面を組み立て、
// hosted repo に上がったバイト列を返す。packager は呼ばれたら失敗する —— 作り直しは禁じ手だから。
func aptFromRelease(t *testing.T, set build.ArtifactSet, debName string, debBytes []byte) (output.Result, [][]byte) {
	t.Helper()
	root := scratchModule(t) // .wharfy/dist も .wharfy/artifacts.json も無い = 別ジョブの runner
	writeChannels(t, root, "project: demo\ngithub: acme/app\nchannels: [apt]\napt:\n  repo: https://pkg.example.com/acme/repo\n")
	tagScratch(t, root, "v0.4.0")
	chdir(t, root)
	t.Setenv("PACKAGE_REPO_TOKEN", "tok")

	srv := ghPackageServer(t, "v0.4.0", debName, set, debBytes)
	old := newReleasesProbe
	newReleasesProbe = func(owner, repo string) *channel.ReleasesProbe {
		return &channel.ReleasesProbe{Owner: owner, Repo: repo, API: srv.URL}
	}
	t.Cleanup(func() { newReleasesProbe = old })

	t.Cleanup(swapPackager(fakePackager{err: errors.New("packages must not be rebuilt when the release already carries them")}))
	var uploaded [][]byte
	t.Cleanup(swapUploader(func(_ context.Context, _, _, path string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		uploaded = append(uploaded, b)
		return nil
	}))
	flagYes = true
	t.Cleanup(func() { flagYes = false })

	return runPublish(context.Background(), mustLookup(t, "publish"), []string{"apt"}), uploaded
}

// TestPublishAptUploadsTheReleasedPackage: promote 後の publish は dist を持たない(資産を作り直さない
// のが昇格の要 = D-264)。deb/rpm だけは URL ではなく**バイト列そのもの**を hosted repo に渡すので、
// 実ファイルが要る —— それを Release 資産から引く。作り直せば利用者が入れるのは検証したのとは
// 別のバイト列になる(v0.23.0 で apt/rpm が配れず取り残された穴)。
func TestPublishAptUploadsTheReleasedPackage(t *testing.T) {
	debBytes := []byte("the bytes users verified")
	sum := sha256.Sum256(debBytes)
	const debName = "demo_0.4.0_linux_amd64.deb"
	set := build.ArtifactSet{Version: "0.4.0", Artifacts: []build.Artifact{
		{OS: "linux", Arch: "amd64", Path: ".wharfy/dist/" + debName, SHA256: hex.EncodeToString(sum[:])},
	}}

	res, uploaded := aptFromRelease(t, set, debName, debBytes)
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if len(uploaded) != 1 {
		t.Fatalf("the released deb should have been uploaded once, got %d", len(uploaded))
	}
	if !bytes.Equal(uploaded[0], debBytes) {
		t.Errorf("users must get the bytes carried by the release, got %q", uploaded[0])
	}
}

// TestPublishAptRefusesRewrittenPackage: 落とした資産が記録の sha256 と食い違えば、配らずに止める
// (誰かが Release を貼り替えた = 検証したバイト列ではない)。
func TestPublishAptRefusesRewrittenPackage(t *testing.T) {
	const debName = "demo_0.4.0_linux_amd64.deb"
	set := build.ArtifactSet{Version: "0.4.0", Artifacts: []build.Artifact{
		{OS: "linux", Arch: "amd64", Path: ".wharfy/dist/" + debName, SHA256: hex.EncodeToString(sha256.New().Sum(nil))},
	}}

	res, uploaded := aptFromRelease(t, set, debName, []byte("swapped bytes"))
	if res.OK {
		t.Fatalf("a release asset that does not match the record must not be published: %+v", res)
	}
	if len(uploaded) != 0 {
		t.Errorf("nothing may reach the hosted repo, got %d upload(s)", len(uploaded))
	}
	if len(res.Errors) == 0 || !strings.Contains(res.Errors[0].Message, "recorded bytes") {
		t.Errorf("the failure should name the mismatch: %+v", res.Errors)
	}
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
