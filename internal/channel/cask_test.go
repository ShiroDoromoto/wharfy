package channel

import (
	"context"
	"strings"
	"testing"
)

func sampleCask() CaskInput {
	return CaskInput{
		Token:    "demo-app",
		Name:     "Demo",
		Desc:     "a demo GUI",
		Homepage: "https://github.com/acme/demo",
		Version:  "1.2.3",
		Artifacts: []CaskArtifact{
			{Arch: "arm64", URL: "https://x/Demo-1.2.3-arm64.dmg", SHA256: "aa"},
			{Arch: "amd64", URL: "https://x/Demo-1.2.3-amd64.dmg", SHA256: "bb"},
		},
	}
}

// TestGenerateCaskArchSplit: arm/intel を持つと on_arm/on_intel ブロックに分かれ、token・表示名・
// app stanza が出る。非 notarized(既定)なので Gatekeeper 案内の caveats が付く(依頼②/⑤)。
func TestGenerateCaskArchSplit(t *testing.T) {
	got := GenerateCask(sampleCask())
	for _, want := range []string{
		`cask "demo-app" do`,
		`version "1.2.3"`,
		"on_arm do", "on_intel do",
		`url "https://x/Demo-1.2.3-arm64.dmg"`,
		`sha256 "aa"`,
		`name "Demo"`,
		`desc "a demo GUI"`,
		`homepage "https://github.com/acme/demo"`,
		`app "Demo.app"`,
		"caveats <<~EOS",
		"not notarized",
		"end\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cask missing %q\n---\n%s", want, got)
		}
	}
}

// TestGenerateCaskUniversal: universal 単独なら on_arm/on_intel でなく top-level url/sha256 を出す。
func TestGenerateCaskUniversal(t *testing.T) {
	in := sampleCask()
	in.Artifacts = []CaskArtifact{{Arch: "universal", URL: "https://x/Demo-1.2.3-universal.dmg", SHA256: "cc"}}
	got := GenerateCask(in)
	if strings.Contains(got, "on_arm") || strings.Contains(got, "on_intel") {
		t.Errorf("universal should not split arch:\n%s", got)
	}
	if !strings.Contains(got, `  url "https://x/Demo-1.2.3-universal.dmg"`) || !strings.Contains(got, `  sha256 "cc"`) {
		t.Errorf("universal should emit top-level url/sha256:\n%s", got)
	}
}

// TestGenerateCaskAppDefaultsToName: app stanza 未指定なら "<表示名>.app"、明示なら上書き(依頼⑥)。
func TestGenerateCaskAppDefaults(t *testing.T) {
	in := sampleCask()
	in.AppBundle = ""
	if !strings.Contains(GenerateCask(in), `app "Demo.app"`) {
		t.Error("app stanza should default to <name>.app")
	}
	in.AppBundle = "Demo Desktop.app"
	if !strings.Contains(GenerateCask(in), `app "Demo Desktop.app"`) {
		t.Error("explicit app stanza should win")
	}
}

// TestGenerateCaskNotarizedOmitsCaveats: notarize 済みなら Gatekeeper caveats を出さない。
func TestGenerateCaskNotarizedOmitsCaveats(t *testing.T) {
	in := sampleCask()
	in.Notarized = true
	if strings.Contains(GenerateCask(in), "caveats") {
		t.Error("notarized cask should not emit Gatekeeper caveats")
	}
}

// TestCaskVersion: 生成した cask から version を読み戻せる(Probe の照合に使う)。
func TestCaskVersion(t *testing.T) {
	if got := CaskVersion(GenerateCask(sampleCask())); got != "1.2.3" {
		t.Errorf("CaskVersion = %q, want 1.2.3", got)
	}
	if CaskVersion("no version here") != "" {
		t.Error("missing version should return empty")
	}
}

func newCask(store TapStore) *Cask {
	return &Cask{Token: "demo-app", Tap: "acme/homebrew-demo", Store: store, Input: sampleCask()}
}

// TestCaskPlanCreate: 空 tap には create。owned_artifact は Casks/<token>.rb(Formula/ とは別ディレクトリ)。
func TestCaskPlanCreate(t *testing.T) {
	ck := newCask(NewInMemoryTapStore())
	item, err := ck.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item.Action != ActionCreate {
		t.Errorf("action = %q, want create", item.Action)
	}
	if item.OwnedArtifact != "acme/homebrew-demo:Casks/demo-app.rb" {
		t.Errorf("owned_artifact = %q", item.OwnedArtifact)
	}
	if item.Kind != KindOwned || !strings.Contains(item.Diff, `+cask "demo-app"`) {
		t.Errorf("create plan wrong: %+v", item)
	}
}

// TestCaskPlanNoopAndUpdate: 同一内容は noop、版違いは update＋差分。
func TestCaskPlanNoopAndUpdate(t *testing.T) {
	store := NewInMemoryTapStore()
	ck := newCask(store)
	store.Files[ck.CaskPath()] = GenerateCask(ck.Input)
	item, _ := ck.Plan(context.Background())
	if item.Action != ActionNoop || item.Diff != "" {
		t.Errorf("want noop with empty diff, got %+v", item)
	}
	old := ck.Input
	old.Version = "1.0.0"
	store.Files[ck.CaskPath()] = GenerateCask(old)
	item, _ = ck.Plan(context.Background())
	if item.Action != ActionUpdate {
		t.Fatalf("want update, got %q", item.Action)
	}
	if !strings.Contains(item.Diff, `-  version "1.0.0"`) || !strings.Contains(item.Diff, `+  version "1.2.3"`) {
		t.Errorf("update diff should show version change:\n%s", item.Diff)
	}
}

// TestCaskPublishWritesOnlyWhenNeeded: 初回は create+commit、同一再実行は noop(書かない)。
func TestCaskPublishWritesOnlyWhenNeeded(t *testing.T) {
	store := NewInMemoryTapStore()
	ck := newCask(store)
	item, pub, err := ck.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item.Action != ActionCreate || store.Commits != 1 || pub.Commit == "" {
		t.Errorf("first publish should create+commit: action=%q commits=%d commit=%q", item.Action, store.Commits, pub.Commit)
	}
	if store.Files[ck.CaskPath()] != GenerateCask(ck.Input) {
		t.Error("tap should hold the generated cask after publish")
	}
	if _, _, err := ck.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.Commits != 1 {
		t.Errorf("noop re-publish should not commit again: commits=%d", store.Commits)
	}
}

// TestCaskProbe: tap 上の cask 版を読み、未発行は Found=false。
func TestCaskProbe(t *testing.T) {
	store := NewInMemoryTapStore()
	ck := newCask(store)
	rs, err := ck.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rs.Found {
		t.Error("empty tap should probe Found=false")
	}
	store.Files[ck.CaskPath()] = GenerateCask(ck.Input)
	rs, _ = ck.Probe(context.Background())
	if !rs.Found || rs.Version != "1.2.3" {
		t.Errorf("probe = %+v, want found 1.2.3", rs)
	}
}

// TestCaskSharesTapWithFormula: Cask と Formula が同じ tap・同じ Store に別ファイルで同居できる
// (Formula/ と Casks/ でパスが分かれる・状態一元化・依頼④)。
func TestCaskSharesTapWithFormula(t *testing.T) {
	store := NewInMemoryTapStore()
	hb := newHomebrew(store)
	ck := newCask(store)
	if _, _, err := hb.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ck.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Files["Formula/demo.rb"]; !ok {
		t.Error("formula missing from shared tap")
	}
	if _, ok := store.Files["Casks/demo-app.rb"]; !ok {
		t.Error("cask missing from shared tap")
	}
}
