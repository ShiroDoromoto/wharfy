package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// freeze_test.go — ship:false の凍結(D-3 / #21)。
// 守るのは 2 つ。凍結したチャネルに新版が漏れないこと、据え置いた事実が黙って消えないこと。

func frozenCfg(channels ...string) config.Config {
	cfg := config.Config{Project: "demo"}
	for _, ch := range channels {
		cfg.Channels = append(cfg.Channels, config.ResolvedChannel{
			Name: ch, Deprecated: &config.Deprecation{Since: "1.4.0", Ship: false, Message: "moved to brew"},
		})
	}
	return cfg
}

func stateWith(rec map[string]state.PublishRecord) *state.State {
	return &state.State{Project: "demo", Publish: rec}
}

var demoArtifacts = []build.Artifact{
	{OS: "darwin", Arch: "arm64", Path: "dist/demo_1.3.0_darwin_arm64.tar.gz", SHA256: "aa"},
	{OS: "linux", Arch: "amd64", Path: "dist/demo_1.3.0_linux_amd64.tar.gz", SHA256: "bb"},
}

// 配り続けるのは最後に配った版。その版の成果物ごと生成器へ渡せる。
func TestResolveFreezeManifestUsesLastPublished(t *testing.T) {
	st := stateWith(map[string]state.PublishRecord{
		"homebrew": {Version: "1.3.0", Artifacts: demoArtifacts},
	})
	fz := resolveFreeze(frozenCfg("homebrew"), st, "homebrew")
	if fz == nil || fz.Mode != freezeManifest {
		t.Fatalf("homebrew must rebuild its manifest at the frozen version, got %+v", fz)
	}
	if fz.Version != "1.3.0" || len(fz.Artifacts) != 2 {
		t.Fatalf("frozen at %q with %d artifact(s)", fz.Version, len(fz.Artifacts))
	}
	w := freezeWarning(fz)
	if w.Code != output.WarnDeprecateFrozen || !strings.Contains(w.Message, "1.3.0") {
		t.Errorf("the warning must name the frozen version: %+v", w)
	}
}

// 一度も配っていなければ凍結先が無い。新版を配るのではなく、何も配らない。
func TestResolveFreezeHoldsWhenNeverPublished(t *testing.T) {
	fz := resolveFreeze(frozenCfg("homebrew"), stateWith(nil), "homebrew")
	if fz == nil || fz.Mode != freezeHold {
		t.Fatalf("no record → hold, got %+v", fz)
	}
	if !strings.Contains(fz.Reason, "never published") {
		t.Errorf("reason must say why: %q", fz.Reason)
	}
}

// 成果物の記録が無い版は、マニフェストを作り直せない。据え置いて告知は latest.json に任せる。
func TestResolveFreezeHoldsWithoutRecordedArtifacts(t *testing.T) {
	st := stateWith(map[string]state.PublishRecord{"scoop": {Version: "1.3.0"}})
	fz := resolveFreeze(frozenCfg("scoop"), st, "scoop")
	if fz == nil || fz.Mode != freezeHold {
		t.Fatalf("no recorded artifacts → hold, got %+v", fz)
	}
	if !strings.Contains(fz.Reason, "1.3.0") {
		t.Errorf("reason must name the frozen version: %q", fz.Reason)
	}
}

// アップロードした実体を作り直せないチャネルは、書き込みを止めるだけ。
func TestResolveFreezeHoldChannels(t *testing.T) {
	st := stateWith(map[string]state.PublishRecord{"apt": {Version: "1.3.0"}, "winget": {Version: "1.3.0"}})
	for _, ch := range []string{"apt", "winget"} {
		fz := resolveFreeze(frozenCfg(ch), st, ch)
		if fz == nil || fz.Mode != freezeHold {
			t.Fatalf("%s must hold, got %+v", ch, fz)
		}
		if w := freezeWarning(fz); !strings.Contains(w.Message, "1.3.0") {
			t.Errorf("%s: the warning must name the frozen version: %q", ch, w.Message)
		}
	}
}

// wharfy が版を選べないチャネルは凍結できない。できないと言う。
func TestResolveFreezeUnsupported(t *testing.T) {
	for _, ch := range []string{"goinstall", "releases"} {
		fz := resolveFreeze(frozenCfg(ch), stateWith(nil), ch)
		if fz == nil || fz.Mode != freezeUnsupported {
			t.Fatalf("%s cannot be frozen, got %+v", ch, fz)
		}
		if freezeWarning(fz).Code != output.WarnDeprecateFrozen {
			t.Errorf("%s: silence would read as 'frozen'", ch)
		}
	}
}

// 宣言が無い / ship:true のチャネルは凍結の解決すらしない(生成物は 1 バイトも変わらない)。
func TestResolveFreezeNilWhenShipping(t *testing.T) {
	cfg := config.Config{Channels: []config.ResolvedChannel{
		{Name: "homebrew"},
		{Name: "script", Deprecated: &config.Deprecation{Since: "1.4.0", Ship: true}},
	}}
	if fz := resolveFreeze(cfg, stateWith(nil), "homebrew"); fz != nil {
		t.Errorf("undeclared channel resolved a freeze: %+v", fz)
	}
	if fz := resolveFreeze(cfg, stateWith(nil), "script"); fz != nil {
		t.Errorf("ship:true resolved a freeze: %+v", fz)
	}
}

// 凍結していないチャネルには手元の成果物(release の実体)を渡す。凍結中は record を渡し、
// build を一度も呼ばない — それが「新版を配らない」の実体。
func TestFrozenArtifactsSkipsTheBuild(t *testing.T) {
	built := 0
	mk := func() ([]build.Artifact, error) { built++; return []build.Artifact{{OS: "linux"}}, nil }

	got, err := frozenArtifacts(nil, mk)
	if err != nil || len(got) != 1 || built != 1 {
		t.Fatalf("not frozen → build once: %v %d", err, built)
	}
	fz := &channelFreeze{Mode: freezeManifest, Version: "1.3.0", Artifacts: demoArtifacts}
	got, err = frozenArtifacts(fz, mk)
	if err != nil {
		t.Fatal(err)
	}
	if built != 1 {
		t.Errorf("frozen must not build: built %d times", built)
	}
	if len(got) != len(demoArtifacts) {
		t.Errorf("frozen must serve the recorded artifacts, got %d", len(got))
	}
}

func writeState(t *testing.T, root string, st *state.State) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, state.DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(root, st); err != nil {
		t.Fatal(err)
	}
}

// install.sh は sha を持たないので、凍結中も作り直せる。入れる版は据え置き、告知だけが新しくなる。
func TestInstallScriptTargetFreezesTheVersion(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, stateWith(map[string]state.PublishRecord{"script": {Version: "1.3.0"}}))

	v, ship := installScriptTarget(root, frozenCfg("script"), "1.4.0")
	if !ship || v != "1.3.0" {
		t.Fatalf("script must keep installing 1.3.0, got (%q, %v)", v, ship)
	}

	cfg := frozenCfg("script")
	cfg.Github = "acme/demo"
	sh := config.GenerateInstallScript(cfg, v)
	if !strings.Contains(sh, `VERSION="1.3.0"`) {
		t.Error("install.sh must pin the frozen version")
	}
	if !strings.Contains(sh, "moved to brew") {
		t.Error("install.sh must carry the current notice")
	}
}

// 一度も配っていない script は凍結先が無い。install.sh を同梱しない(新版が漏れる方が悪い)。
func TestInstallScriptTargetSkipsWhenNothingWasShipped(t *testing.T) {
	root := t.TempDir()
	writeState(t, root, stateWith(nil))
	if v, ship := installScriptTarget(root, frozenCfg("script"), "1.4.0"); ship || v != "" {
		t.Fatalf("nothing to freeze at → ship nothing, got (%q, %v)", v, ship)
	}
}

// 宣言が無ければ現在の版をそのまま入れる(既存の挙動を変えない)。
func TestInstallScriptTargetUnchangedWhenShipping(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{Project: "demo", Channels: []config.ResolvedChannel{{Name: "script"}}}
	if v, ship := installScriptTarget(root, cfg, "1.4.0"); !ship || v != "1.4.0" {
		t.Fatalf("got (%q, %v)", v, ship)
	}
}
