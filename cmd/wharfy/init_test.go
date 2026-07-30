package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// init の振る舞い: 無ければ作る / あれば確認の上ブロック追記 / 2回目は冪等 / プレビューは書かない。

// 管理ブロックは入口だけを語ること(責務2)。撒かれた本文は wharfy が変わっても追随しないので、
// 仕様・方針(非対話であること、引き金の作法)を一行でも焼き込めば、その場で古い記述になりうる。
func TestManagedBlockIsEntryOnly(t *testing.T) {
	block := managedBlock()
	if !strings.Contains(block, "wharfy agent") {
		t.Errorf("managed block does not point at the capability map\n---\n%s", block)
	}
	// 変わりうる話の痕跡。ここに 1 つでも当たれば、それは撒かれた先で陳腐化する。
	for _, stale := range []string{"--yes", "tag push", "every merge", "Dependabot", "GitHub Actions", "credential"} {
		if strings.Contains(block, stale) {
			t.Errorf("managed block bakes in a volatile detail %q (it belongs in `wharfy agent` notes)\n---\n%s", stale, block)
		}
	}
}

// 管理ブロックは入口の二言だけ(責務2の続き)。禁止形・戒めの調子を混ぜないこと——読むのは指示に
// 従うエージェントで、本文に無い制約まで方針として読み取る。現に「自動配布しない」と読まれ、CI を
// 使うか使わないかという利用者の判断にまで及んだ。何をどう回すかは利用者が決める。
func TestManagedBlockKeepsNoPolicyVoice(t *testing.T) {
	block := managedBlock()

	// 見出し・空行・マーカーを除いた本文。二言(2 行)を超えたら、入口以外の何かを言い始めている。
	body := make([]string, 0, 2)
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "<!--") {
			continue
		}
		body = append(body, line)
	}
	if len(body) > 2 {
		t.Errorf("managed block says more than the two lines it owes (%d)\n---\n%s", len(body), block)
	}

	// 指図の匂い。1 つでも当たれば、撒かれた先で方針として読まれる。
	lower := strings.ToLower(block)
	for _, voice := range []string{"don't", "do not", "never", "must", "should", "always", "ci "} {
		if strings.Contains(lower, voice) {
			t.Errorf("managed block tells the reader what to do or not do (%q)\n---\n%s", voice, block)
		}
	}
}

// 管理ブロックが持たない事実——wharfy は非対話で、手元でも CI でも同じコマンドが動く——が、能力マップ側
// (常に現行版)で語られること。配布は身構えられがちで、これを知らないと CI で回す道が最初から消える。
// 逆に「いつ引き金を引くか」は利用者が決めることなので、ここでも指図しない。
func TestNonInteractiveFactLivesInAgentNotes(t *testing.T) {
	for _, name := range []string{"release", "publish"} {
		spec, ok := registry.Lookup(name)
		if !ok {
			t.Fatalf("registry has no %q command", name)
		}
		notes := strings.Join(spec.Notes, "\n")
		for _, want := range []string{"non-interactive", "GitHub Actions workflow"} {
			if !strings.Contains(notes, want) {
				t.Errorf("%s notes no longer say wharfy runs unattended (%q)\n---\n%s", name, want, notes)
			}
		}
		for _, unwanted := range []string{"never on every merge", "stays deliberate"} {
			if strings.Contains(notes, unwanted) {
				t.Errorf("%s notes tell the user when to ship (%q) — that is theirs to decide\n---\n%s", name, unwanted, notes)
			}
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

// 古い wharfy が書いたブロックが残っている状態。「有る」だけで黙ると、エージェントは古い入口を
// 読み続ける——status は init_stale で言い、init に回す。
func TestStatusFlagsStaleBlock(t *testing.T) {
	root := scratchModule(t)
	chdir(t, root)

	stale := initBeginMarker + "\n## Releasing\n\nold body from a previous wharfy.\n" + initEndMarker + "\n"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := buildStatus(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if hasWarn(out, "init_missing") {
		t.Errorf("a stale block is not a missing one: %+v", out.Warnings)
	}
	if !hasWarn(out, "init_stale") {
		t.Errorf("stale block should warn init_stale: %+v", out.Warnings)
	}
	if !hasNextDoOut(out, "wharfy init --yes") {
		t.Errorf("stale block should suggest wharfy init --yes: %+v", out.Next)
	}

	// 差し替えれば黙る(wharfy init が begin/end で冪等に直すのと同じ本文)。
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(managedBlock()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = buildStatus(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if hasWarn(out, "init_stale") {
		t.Errorf("refreshed block must not warn init_stale: %+v", out.Warnings)
	}
}

// 片方だけ古い混在。新しい方が在っても、古い方を読むエージェントは古い入口を読む——だから言う。
func TestAgentInstructionsStateStaleWins(t *testing.T) {
	withTempDir(t)

	if err := os.WriteFile("CLAUDE.md", []byte(managedBlock()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := initBeginMarker + "\n## Releasing\n\nold body.\n" + initEndMarker + "\n"
	if err := os.WriteFile("AGENTS.md", []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := agentInstructionsState(); got != instructionsStale {
		t.Errorf("one stale file should make the whole state stale: got %v", got)
	}
}

// withInitNudge は管理ブロックが無い/古いなら成功 Result に促しを足し、現行なら素通しする。
func TestWithInitNudge(t *testing.T) {
	withTempDir(t)

	res := withInitNudge(output.New("release", "released", true))
	if !hasResWarn(res, "init_missing") || !hasResNext(res, "wharfy init --yes") {
		t.Errorf("missing init should add nudge: %+v", res)
	}

	stale := initBeginMarker + "\n## Releasing\n\nold body.\n" + initEndMarker + "\n"
	if err := os.WriteFile("AGENTS.md", []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	res = withInitNudge(output.New("release", "released", true))
	if !hasResWarn(res, "init_stale") || !hasResNext(res, "wharfy init --yes") {
		t.Errorf("stale block should add nudge: %+v", res)
	}

	if err := os.WriteFile("AGENTS.md", []byte(managedBlock()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = withInitNudge(output.New("release", "released", true))
	if hasResWarn(res, "init_missing") || hasResWarn(res, "init_stale") || hasResNext(res, "wharfy init --yes") {
		t.Errorf("with a current block, nudge must not be added: %+v", res)
	}
}

// 促しは言うだけ——release/publish の副作用で利用者のファイルを黙って書き換えない(書くのは init だけ)。
func TestNudgeNeverWritesFiles(t *testing.T) {
	withTempDir(t)

	stale := initBeginMarker + "\n## Releasing\n\nold body.\n" + initEndMarker + "\n"
	if err := os.WriteFile("AGENTS.md", []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	withInitNudge(output.New("release", "released", true))
	got, err := os.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != stale {
		t.Errorf("the nudge rewrote the file; only wharfy init may write it\n---\n%s", got)
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
