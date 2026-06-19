package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// fakePackager は goreleaser を起動せず固定の deb/rpm 成果物を返す。
type fakePackager struct {
	arts []build.Artifact
	err  error
}

func (f fakePackager) Packages(context.Context, string, string) ([]build.Artifact, error) {
	return f.arts, f.err
}

func swapPackager(p build.Packager) func() {
	prev := newPackager
	newPackager = func(string) build.Packager { return p }
	return func() { newPackager = prev }
}

func swapUploader(fn func(context.Context, string, string, string) error) func() {
	prev := uploadPackage
	uploadPackage = fn
	return func() { uploadPackage = prev }
}

// repo 未設定 → skip して案内(channel_skipped)。
func TestPublishAptSkipNoRepo(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [apt]\n") // apt.repo 未設定
	chdir(t, root)

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"apt"})
	if !res.OK {
		t.Fatalf("skip is not a failure: %+v", res)
	}
	if res.Data.(publishData).Plan[0].Action != channel.ActionSkip {
		t.Errorf("no repo → skip: %+v", res.Data)
	}
	if !hasWarning(res, "channel_skipped") {
		t.Errorf("should warn channel_skipped: %+v", res.Warnings)
	}
}

// dry-run: 生成される deb を列挙し、PACKAGE_REPO_TOKEN を前提に出す。
func TestPublishAptDryRun(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [apt]\napt:\n  repo: https://pkg.example.com/acme/repo\n")
	chdir(t, root)
	t.Setenv("PACKAGE_REPO_TOKEN", "")

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"apt"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	pd := res.Data.(publishData)
	if pd.Applied || pd.Plan[0].OwnedArtifact != "https://pkg.example.com/acme/repo" {
		t.Errorf("plan target wrong: %+v", pd.Plan[0])
	}
	if !strings.Contains(pd.Plan[0].Diff, "demo_0.0.0_linux_amd64.deb") {
		t.Errorf("diff should list expected debs: %q", pd.Plan[0].Diff)
	}
	if !requirementUnmet(pd.Requires, "PACKAGE_REPO_TOKEN") {
		t.Errorf("PACKAGE_REPO_TOKEN should be an unmet requirement: %+v", pd.Requires)
	}
}

// --yes: nfpm パッケージ(fake)を hosted repo へアップロードし、state に記録。
func TestPublishAptApply(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [apt]\napt:\n  repo: https://pkg.example.com/acme/repo\n")
	tagScratch(t, root, "v0.4.0")
	chdir(t, root)
	t.Setenv("PACKAGE_REPO_TOKEN", "tok")

	// fake パッケージ(実ファイルは不要。uploader も fake にする)。
	deb := filepath.Join(".wharfy", "dist", "demo_0.4.0_linux_amd64.deb")
	defer swapPackager(fakePackager{arts: []build.Artifact{
		{OS: "linux", Arch: "amd64", Path: deb, SHA256: "aa"},
		{OS: "linux", Arch: "amd64", Path: ".wharfy/dist/demo_0.4.0_linux_amd64.rpm", SHA256: "bb"}, // rpm は apt で無視
	}})()
	var got []string
	defer swapUploader(func(_ context.Context, repo, token, path string) error {
		got = append(got, repo+"|"+token+"|"+filepath.Base(path))
		return nil
	})()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"apt"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	if !res.Data.(publishData).Applied {
		t.Errorf("expected applied: %+v", res.Data)
	}
	// .deb だけ、正しい repo/token でアップロードされる。
	if len(got) != 1 || got[0] != "https://pkg.example.com/acme/repo|tok|demo_0.4.0_linux_amd64.deb" {
		t.Errorf("uploaded calls wrong: %v", got)
	}
	st, _ := state.Load(root, "demo")
	if _, ok := st.Publish["apt"]; !ok {
		t.Error("apt publish should be recorded")
	}
}

// provider: fury — upload は push.fury.io へ、記録(状態)は配信 URL(apt.fury.io)になる。
func TestPublishAptApplyFury(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [apt]\napt:\n  provider: fury\n  user: shiro\n")
	tagScratch(t, root, "v0.4.0")
	chdir(t, root)
	t.Setenv("PACKAGE_REPO_TOKEN", "tok")

	deb := filepath.Join(".wharfy", "dist", "demo_0.4.0_linux_amd64.deb")
	defer swapPackager(fakePackager{arts: []build.Artifact{
		{OS: "linux", Arch: "amd64", Path: deb, SHA256: "aa"},
	}})()
	var got []string
	defer swapUploader(func(_ context.Context, repo, token, path string) error {
		got = append(got, repo+"|"+token+"|"+filepath.Base(path))
		return nil
	})()
	defer func() { flagYes = false }()
	flagYes = true

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"apt"})
	if !res.OK {
		t.Fatalf("expected ok: %+v", res)
	}
	// upload は push ホストへ。
	if len(got) != 1 || got[0] != "https://push.fury.io/shiro/|tok|demo_0.4.0_linux_amd64.deb" {
		t.Errorf("upload should go to push host: %v", got)
	}
	// 記録は配信 URL(probe/install 用)。
	st, _ := state.Load(root, "demo")
	if rec := st.Publish["apt"]; rec.Target != "https://apt.fury.io/shiro/" {
		t.Errorf("recorded target should be delivery URL, got %q", rec.Target)
	}
}

// repo 未設定 → skip 時に「どこにホストするか」の選択ガイド(fury 推奨)を next: で案内する。
func TestPublishAptSkipGuide(t *testing.T) {
	root := scratchModule(t)
	writeChannels(t, root, "project: demo\nchannels: [apt]\n")
	chdir(t, root)

	res := runPublish(context.Background(), mustLookup(t, "publish"), []string{"apt"})
	if !res.OK || !hasWarning(res, "channel_skipped") {
		t.Fatalf("expected ok skip with channel_skipped: %+v", res)
	}
	var sawFury bool
	for _, n := range res.Next {
		if strings.Contains(n.Do, "provider: fury") {
			sawFury = true
		}
	}
	if !sawFury {
		t.Errorf("guide should recommend fury provider: %+v", res.Next)
	}
}

// httpUploadPackage: multipart POST + basic auth を実サーバ(httptest)で確認。
func TestHTTPUploadPackage(t *testing.T) {
	tmp := t.TempDir()
	pkg := filepath.Join(tmp, "demo_1.0.0_linux_amd64.deb")
	if err := os.WriteFile(pkg, []byte("DEB-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotUser, gotField, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		gotUser = user
		f, h, err := r.FormFile("package")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotField = h.Filename
		b := make([]byte, 64)
		n, _ := f.Read(b)
		gotBody = string(b[:n])
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	if err := httpUploadPackage(context.Background(), srv.URL, "mytoken", pkg); err != nil {
		t.Fatal(err)
	}
	if gotUser != "mytoken" || gotField != "demo_1.0.0_linux_amd64.deb" || gotBody != "DEB-CONTENT" {
		t.Errorf("upload mismatch: user=%q field=%q body=%q", gotUser, gotField, gotBody)
	}
}
