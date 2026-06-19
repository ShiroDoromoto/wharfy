package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

const resultSchemaID = "https://wharfy.io/schemas/v1/result.json"

// fakeBuilder は goreleaser を起動せず固定の成果物/エラーを返す(末端は差し替え可能)。
type fakeBuilder struct {
	arts []build.Artifact
	err  error
}

func (f fakeBuilder) Build(context.Context, string, string) ([]build.Artifact, error) {
	return f.arts, f.err
}

// TestBuildResultValidatesSchema: build の envelope が result.json に valid(build は汎用 envelope)。
func TestBuildResultValidatesSchema(t *testing.T) {
	res := output.New("build", "built 1 artifact → .wharfy/dist", true)
	res.Data = buildData{Artifacts: []build.Artifact{{
		OS: "linux", Arch: "amd64", Path: ".wharfy/dist/x/tool",
		SHA256: "78ef2018d6a8fedef7fcfe7c492543b8f91772b85cb7c6423ed430af3ac36d26",
	}}}
	res.Next = []output.NextDo{{Reason: "sign", Do: "wharfy sign"}}
	validateAgainst(t, resultSchemaID, res)
}

// scratchModule は go list / git remote が効く最小モジュールを temp に作る。
func scratchModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module github.com/acme/demo\n\ngo 1.22.0\n")
	write("cmd/demo/main.go", "package main\n\nfunc main() {}\n")
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/acme/demo.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

// TestBuildWiringSuccess: runBuild が解決→生成→(fake)ビルド→記録まで通し、ok と next を返す。
func TestBuildWiringSuccess(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)

	arts := []build.Artifact{
		{OS: "linux", Arch: "amd64", Path: ".wharfy/dist/x/demo", SHA256: "deadbeef"},
		{OS: "darwin", Arch: "arm64", Path: ".wharfy/dist/y/demo", SHA256: "cafef00d"},
	}
	restore := swapBuilder(fakeBuilder{arts: arts})
	defer restore()

	res := runBuild(context.Background(), mustLookup(t, "build"), nil)
	if !res.OK {
		t.Fatalf("expected ok, got: %+v", res)
	}
	bd, ok := res.Data.(buildData)
	if !ok || len(bd.Artifacts) != 2 {
		t.Fatalf("expected 2 artifacts in data, got %+v", res.Data)
	}
	if !hasNextDo(res, "wharfy sign") || !hasNextDo(res, "wharfy publish") {
		t.Errorf("next should include sign and publish: %+v", res.Next)
	}
	// 生成設定と状態が .wharfy/ に書かれる(root は汚さない)。
	if _, err := os.Stat(filepath.Join(root, ".wharfy", "goreleaser.yaml")); err != nil {
		t.Errorf("generated config missing: %v", err)
	}
	st, err := state.Load(root, "demo")
	if err != nil || st.Build == nil || len(st.Build.Artifacts) != 2 {
		t.Errorf("build not recorded in state: %+v err=%v", st, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".goreleaser.yaml")); !os.IsNotExist(err) {
		t.Error("must not write .goreleaser.yaml at repo root")
	}
}

// TestBuildWiringUnavailable: Builder が UnavailableError なら builder_unavailable で停止。
func TestBuildWiringUnavailable(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)
	restore := swapBuilder(fakeBuilder{err: &build.UnavailableError{Bin: "goreleaser"}})
	defer restore()

	res := runBuild(context.Background(), mustLookup(t, "build"), nil)
	if res.OK {
		t.Fatal("expected failure")
	}
	if len(res.Errors) == 0 || res.Errors[0].Code != output.ErrBuilderUnavailable {
		t.Errorf("expected builder_unavailable, got %+v", res.Errors)
	}
}

// --- helpers ---

// chdir は作業ディレクトリを移し、テスト終了時に必ず戻す(go1.22 互換。t.Chdir は 1.24+)。
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func swapBuilder(b build.Builder) func() {
	prev := newBuilder
	newBuilder = func(string) build.Builder { return b }
	return func() { newBuilder = prev }
}

func hasNextDo(res output.Result, do string) bool {
	for _, n := range res.Next {
		if n.Do == do {
			return true
		}
	}
	return false
}
