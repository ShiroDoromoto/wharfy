package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

const selfModule = "github.com/ShiroDoromoto/wharfy"

// 判定材料が欠けたら黙る、が要点。他人のリリースを誤検知で騒がせない。
func TestStaleGeneratorMessage(t *testing.T) {
	head := "1111111111111111111111111111111111111111"
	old := "2222222222222222222222222222222222222222"

	cases := []struct {
		name                                string
		selfMod, selfRev, repoMod, repoHead string
		wantWarn                            bool
		wantContains                        string
	}{
		{name: "他人の repo は対象外", selfMod: selfModule, selfRev: old, repoMod: "example.com/app", repoHead: head},
		{name: "go.mod が無い repo も対象外", selfMod: selfModule, selfRev: old, repoMod: "", repoHead: head},
		{name: "実行中バイナリの module 不明なら黙る", selfMod: "", selfRev: old, repoMod: selfModule, repoHead: head},
		{name: "git が無ければ比べられない", selfMod: selfModule, selfRev: old, repoMod: selfModule, repoHead: ""},
		{name: "HEAD からビルドされていれば黙る", selfMod: selfModule, selfRev: head, repoMod: selfModule, repoHead: head},
		{
			name: "旧 commit のバイナリで自分をリリース", selfMod: selfModule, selfRev: old, repoMod: selfModule, repoHead: head,
			wantWarn: true, wantContains: "2222222",
		},
		{
			name: "素性不明のバイナリ(go install など)", selfMod: selfModule, selfRev: "", repoMod: selfModule, repoHead: head,
			wantWarn: true, wantContains: "no build revision",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := staleGeneratorMessage(tc.selfMod, tc.selfRev, tc.repoMod, tc.repoHead)
			if tc.wantWarn == (got == "") {
				t.Fatalf("warn=%v but message=%q", tc.wantWarn, got)
			}
			if tc.wantContains != "" && !strings.Contains(got, tc.wantContains) {
				t.Errorf("message %q does not contain %q", got, tc.wantContains)
			}
		})
	}
}

func TestModuleOfDir(t *testing.T) {
	dir := t.TempDir()
	if got := moduleOfDir(dir); got != "" {
		t.Errorf("go.mod なしで %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("// c\n\nmodule "+selfModule+"\n\ngo 1.26.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := moduleOfDir(dir); got != selfModule {
		t.Errorf("module = %q, want %q", got, selfModule)
	}
}

func TestGitHeadRevision(t *testing.T) {
	dir := t.TempDir()
	if got := gitHeadRevision(dir); got != "" {
		t.Errorf("git repo でないのに %q", got)
	}
	for _, args := range [][]string{
		{"init"}, {"commit", "--allow-empty", "-m", "x", "--no-gpg-sign"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if got := gitHeadRevision(dir); len(got) != 40 {
		t.Errorf("HEAD = %q, want a 40-char sha", got)
	}
}

// 版ズレを見つけたら warning と「HEAD からビルドし直す」next の両方を足す。
func TestWithStaleGeneratorWarning(t *testing.T) {
	restore := func(m, r func() string) { selfModulePath, selfRevision = m, r }
	defer restore(selfModulePath, selfRevision)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+selfModule+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// gitHeadRevision は root で git を呼ぶ。repo でなければ空 → 黙る。
	selfModulePath = func() string { return selfModule }
	selfRevision = func() string { return "dead" }
	c := registry.Command{Name: "release"}
	if res := withStaleGeneratorWarning(root, c, output.New(c.Name, "ok", true)); len(res.Warnings) != 0 {
		t.Fatalf("git repo でないのに warning: %+v", res.Warnings)
	}

	// 実 repo(このソースツリー)なら HEAD が読めるので、素性の違うバイナリは検知される。
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(filepath.Dir(root)) // cmd/wharfy → repo root(go.mod のある所)
	res := withStaleGeneratorWarning(root, c, output.New(c.Name, "ok", true))
	if len(res.Warnings) != 1 || res.Warnings[0].Code != output.WarnStaleGenerator {
		t.Fatalf("warnings = %+v, want 1 stale_generator", res.Warnings)
	}
	if len(res.Next) != 1 || !strings.Contains(res.Next[0].Do, "go build -o /tmp/wharfy") {
		t.Errorf("next = %+v, want a rebuild-from-HEAD hint", res.Next)
	}

	// HEAD からビルドされたバイナリなら何も足さない(開発中の使い捨てバイナリはこの経路)。
	selfRevision = func() string { return gitHeadRevision(root) }
	if res := withStaleGeneratorWarning(root, c, output.New(c.Name, "ok", true)); len(res.Warnings) != 0 {
		t.Errorf("HEAD 由来なのに warning: %+v", res.Warnings)
	}
}

// apply(--yes)は版ズレのまま走らせない。警告はアップロード後にしか読まれないので、
// 手前で止めるのが唯一の防波堤。逃げ道は --allow-stale-generator だけ。
func TestStaleGeneratorRefusal(t *testing.T) {
	restore := func(m, r func() string, allow bool) { selfModulePath, selfRevision, flagAllowStale = m, r, allow }
	defer restore(selfModulePath, selfRevision, flagAllowStale)

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(filepath.Dir(root)) // cmd/wharfy → repo root
	c := registry.Command{Name: "release"}

	selfModulePath = func() string { return selfModule }
	selfRevision = func() string { return "dead" }
	flagAllowStale = false
	res, blocked := staleGeneratorRefusal(root, c)
	if !blocked {
		t.Fatal("版ズレなのに apply を通した")
	}
	if res.OK {
		t.Error("拒否なのに ok=true")
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != output.ErrStaleGeneratorBlocked {
		t.Fatalf("errors = %+v, want 1 stale_generator_blocked", res.Errors)
	}
	if len(res.Next) != 2 || !strings.Contains(res.Next[1].Do, "--allow-stale-generator") {
		t.Errorf("next = %+v, want a rebuild hint and an override hint", res.Next)
	}

	// 明示上書き: 進ませる(判断は人間が済ませた)。警告は withStaleGeneratorWarning が結果に残す。
	flagAllowStale = true
	if _, blocked := staleGeneratorRefusal(root, c); blocked {
		t.Error("--allow-stale-generator なのに拒否した")
	}

	// HEAD 由来のバイナリ、および wharfy 以外の repo では発火しない。
	flagAllowStale = false
	selfRevision = func() string { return gitHeadRevision(root) }
	if _, blocked := staleGeneratorRefusal(root, c); blocked {
		t.Error("HEAD 由来なのに拒否した")
	}
	selfModulePath = func() string { return "example.com/other" }
	selfRevision = func() string { return "dead" }
	if _, blocked := staleGeneratorRefusal(root, c); blocked {
		t.Error("他人の repo で拒否した")
	}
}
