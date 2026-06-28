package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/output"
)

// init の振る舞い: 無ければ作る / あれば確認の上ブロック追記 / 2回目は冪等 / プレビューは書かない。

// 管理ブロックに「マージ≠配布」作法(自動配布禁止・配布はゲート)が含まれること(責務2)。
func TestManagedBlockGatingDiscipline(t *testing.T) {
	block := managedBlock()
	for _, want := range []string{"never auto-distribute", "wharfy release", "wharfy publish", "unattended"} {
		if !strings.Contains(block, want) {
			t.Errorf("managed block missing gating discipline %q\n---\n%s", want, block)
		}
	}
}

// withTempDir は temp dir に chdir し、init のグローバルフラグを毎回リセットする。
func withTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	flagYes, flagDryRun, flagJSON = false, false, false
	// 既定は「非 TTY」(プレビューに倒れる)。対話を試す test だけ true にする。
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() {
		flagYes, flagDryRun, flagJSON = false, false, false
		stdinIsTTY = func() bool { return false }
		promptConfirm = nil
	})
	return dir
}

func TestPlanFile(t *testing.T) {
	if _, action := planFile("", false); action != "create" {
		t.Errorf("absent file: got %q, want create", action)
	}
	if _, action := planFile("# My project\n", true); action != "append" {
		t.Errorf("existing without block: got %q, want append", action)
	}
	// 自分のブロックが既にあり同一 → unchanged。
	withBlock := "# Top\n\n" + managedBlock() + "\n"
	if _, action := planFile(withBlock, true); action != "unchanged" {
		t.Errorf("identical block: got %q, want unchanged", action)
	}
	// ブロック内が古い → update、かつ前後は保たれる。
	stale := "# Top\n\n" + initBeginMarker + "\nOLD BODY\n" + initEndMarker + "\n\n## Tail\n"
	content, action := planFile(stale, true)
	if action != "update" {
		t.Fatalf("stale block: got %q, want update", action)
	}
	if !strings.Contains(content, "# Top") || !strings.Contains(content, "## Tail") {
		t.Errorf("update did not preserve surrounding text:\n%s", content)
	}
	if strings.Contains(content, "OLD BODY") {
		t.Errorf("update kept stale body:\n%s", content)
	}
}

func TestRunInitCreatesFiles(t *testing.T) {
	withTempDir(t)
	flagYes = true

	res := runInit(context.Background(), mustLookup(t, "init"), nil)
	if !res.OK {
		t.Fatalf("init failed: %+v", res)
	}
	validateAgainst(t, "https://wharfy.io/schemas/v1/result.json", res)

	for _, name := range initTargets {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("expected %s written: %v", name, err)
		}
		if !strings.Contains(string(b), "wharfy agent") || !strings.Contains(string(b), initBeginMarker) {
			t.Errorf("%s missing managed block:\n%s", name, b)
		}
	}
}

func TestRunInitIdempotent(t *testing.T) {
	withTempDir(t)
	flagYes = true

	runInit(context.Background(), mustLookup(t, "init"), nil)
	before, _ := os.ReadFile("AGENTS.md")

	res := runInit(context.Background(), mustLookup(t, "init"), nil)
	after, _ := os.ReadFile("AGENTS.md")

	if string(before) != string(after) {
		t.Errorf("second run changed the file:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
	data, _ := res.Data.(map[string]any)
	if data["applied"] != false {
		t.Errorf("second run should report applied=false, got %v", data["applied"])
	}
}

func TestRunInitAppendsToExisting(t *testing.T) {
	withTempDir(t)
	flagYes = true
	original := "# Existing guide\n\nSome rules here.\n"
	if err := os.WriteFile("AGENTS.md", []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	runInit(context.Background(), mustLookup(t, "init"), nil)

	got, _ := os.ReadFile("AGENTS.md")
	if !strings.HasPrefix(string(got), original) {
		t.Errorf("append clobbered existing content:\n%s", got)
	}
	if !strings.Contains(string(got), initBeginMarker) {
		t.Errorf("append did not add the block:\n%s", got)
	}
}

func TestRunInitPreviewWritesNothing(t *testing.T) {
	withTempDir(t) // 非 TTY・--yes 無し → プレビュー
	res := runInit(context.Background(), mustLookup(t, "init"), nil)
	if !res.OK {
		t.Fatalf("preview should be ok: %+v", res)
	}
	if _, err := os.Stat("AGENTS.md"); !os.IsNotExist(err) {
		t.Errorf("preview must not write files")
	}
	if res.Next[0].Do != "wharfy init --yes" {
		t.Errorf("preview should suggest --yes, got %q", res.Next[0].Do)
	}
}

func TestRunInitTTYDeclined(t *testing.T) {
	withTempDir(t)
	stdinIsTTY = func() bool { return true }
	promptConfirm = func(string) (bool, error) { return false, nil }

	res := runInit(context.Background(), mustLookup(t, "init"), nil)
	if !res.OK {
		t.Fatalf("decline should be a clean exit: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(".", "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("declining the prompt must not write files")
	}
}

// status は init 未実施を検出して非致命の促しを出す。block があれば出さない。
func TestStatusNudgesWhenInitMissing(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)

	out, err := buildStatus(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarn(out, "init_missing") {
		t.Errorf("missing init should warn init_missing: %+v", out.Warnings)
	}
	if !hasNextDoOut(out, "wharfy init --yes") {
		t.Errorf("missing init should suggest wharfy init --yes: %+v", out.Next)
	}

	// 管理ブロックを置けば促しは消える。
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(managedBlock()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = buildStatus(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if hasWarn(out, "init_missing") {
		t.Errorf("with block present, init_missing must not fire: %+v", out.Warnings)
	}
}

// withInitNudge は init 未実施なら成功 Result に促しを足し、済みなら素通しする。
func TestWithInitNudge(t *testing.T) {
	withTempDir(t)

	res := withInitNudge(output.New("release", "released", true))
	if !hasResWarn(res, "init_missing") || !hasResNext(res, "wharfy init --yes") {
		t.Errorf("missing init should add nudge: %+v", res)
	}

	if err := os.WriteFile("AGENTS.md", []byte(managedBlock()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = withInitNudge(output.New("release", "released", true))
	if hasResWarn(res, "init_missing") || hasResNext(res, "wharfy init --yes") {
		t.Errorf("with block present, nudge must not be added: %+v", res)
	}
}

func hasResWarn(res output.Result, code string) bool {
	for _, w := range res.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func hasResNext(res output.Result, do string) bool {
	for _, n := range res.Next {
		if n.Do == do {
			return true
		}
	}
	return false
}

func hasWarn(out statusOutput, code string) bool {
	for _, w := range out.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func TestRunInitTTYConfirmed(t *testing.T) {
	withTempDir(t)
	stdinIsTTY = func() bool { return true }
	promptConfirm = func(string) (bool, error) { return true, nil }

	runInit(context.Background(), mustLookup(t, "init"), nil)
	if _, err := os.Stat("AGENTS.md"); err != nil {
		t.Errorf("confirming the prompt should write files: %v", err)
	}
}
