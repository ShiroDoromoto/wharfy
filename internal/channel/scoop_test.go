package channel

import (
	"context"
	"encoding/json"
	"testing"
)

func scoopInput() ScoopInput {
	return ScoopInput{
		Project:     "widget",
		Description: "a widget",
		Homepage:    "https://github.com/acme/widget-demo",
		License:     "MIT",
		Version:     "1.2.3",
		Owner:       "acme",
		Repo:        "widget-demo",
		Archives: []ScoopArch{
			{Arch: "amd64", URL: "https://x/widget_1.2.3_windows_amd64.zip", SHA256: "aa"},
			{Arch: "arm64", URL: "https://x/widget_1.2.3_windows_arm64.zip", SHA256: "bb"},
		},
	}
}

func TestGenerateScoopManifest(t *testing.T) {
	s := GenerateScoopManifest(scoopInput())
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("manifest not valid JSON: %v\n%s", err, s)
	}
	if m["version"] != "1.2.3" || m["license"] != "MIT" || m["checkver"] != "github" {
		t.Errorf("top-level fields wrong: %v", m)
	}
	arch := m["architecture"].(map[string]any)
	a64 := arch["64bit"].(map[string]any)
	if a64["url"] != "https://x/widget_1.2.3_windows_amd64.zip" || a64["hash"] != "aa" || a64["bin"] != "widget.exe" {
		t.Errorf("64bit entry wrong: %v", a64)
	}
	if _, ok := arch["arm64"].(map[string]any); !ok {
		t.Errorf("arm64 entry missing: %v", arch)
	}
	auto := m["autoupdate"].(map[string]any)["architecture"].(map[string]any)
	if au := auto["64bit"].(map[string]any)["url"]; au != "https://github.com/acme/widget-demo/releases/download/v$version/widget_$version_windows_amd64.zip" {
		t.Errorf("autoupdate url wrong: %v", au)
	}
}

func TestManifestVersion(t *testing.T) {
	if v := ManifestVersion(GenerateScoopManifest(scoopInput())); v != "1.2.3" {
		t.Errorf("ManifestVersion = %q, want 1.2.3", v)
	}
	if v := ManifestVersion("not json"); v != "" {
		t.Errorf("ManifestVersion of junk = %q", v)
	}
}

func TestGenerateScoopManifestDependencies(t *testing.T) {
	in := scoopInput()
	in.Dependencies = []string{"nodejs", "git"} // 非ソート入力
	var m map[string]any
	if err := json.Unmarshal([]byte(GenerateScoopManifest(in)), &m); err != nil {
		t.Fatalf("manifest not valid JSON: %v", err)
	}
	deps, ok := m["depends"].([]any)
	if !ok || len(deps) != 2 || deps[0] != "git" || deps[1] != "nodejs" {
		t.Errorf("depends should be sorted [git, nodejs], got %v", m["depends"])
	}
	// 依存無しは depends を出さない(omitempty・後方互換)。
	if _, ok := mapFromJSON(t, GenerateScoopManifest(scoopInput()))["depends"]; ok {
		t.Errorf("no deps should omit depends")
	}
}

func mapFromJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	return m
}

func newScoop(store TapStore) *Scoop {
	return &Scoop{Project: "widget", Bucket: "acme/scoop-widget", Store: store, Input: scoopInput()}
}

func TestScoopPlanCreate(t *testing.T) {
	sc := newScoop(NewInMemoryTapStore())
	item, err := sc.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item.Action != ActionCreate {
		t.Errorf("action = %q, want create", item.Action)
	}
	if item.OwnedArtifact != "acme/scoop-widget:bucket/widget.json" {
		t.Errorf("owned_artifact = %q", item.OwnedArtifact)
	}
}

func TestScoopPublishAndProbe(t *testing.T) {
	store := NewInMemoryTapStore()
	sc := newScoop(store)
	item, pub, err := sc.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item.Action != ActionCreate || store.Commits != 1 || pub.Commit == "" {
		t.Errorf("first publish should create+commit: %+v commits=%d", item, store.Commits)
	}
	// 冪等: 同一内容は書かない。
	if _, _, _ = sc.Publish(context.Background()); store.Commits != 1 {
		t.Errorf("noop publish must not write; commits = %d", store.Commits)
	}
	rs, _ := sc.Probe(context.Background())
	if !rs.Found || rs.Version != "1.2.3" {
		t.Errorf("probe = %+v, want found 1.2.3", rs)
	}
}

// TestGenerateScoopAppManifest: GUI(App=true)は portable zip を参照し、top-level bin ＋ shortcuts を
// 出す。per-arch bin と autoupdate は出さない(持ち込み zip・命名任意・依頼③)。
func TestGenerateScoopAppManifest(t *testing.T) {
	in := scoopInput()
	in.App = true
	in.AppName = "Widget"
	in.ExeName = "Widget.exe"
	in.Owner, in.Repo = "acme", "widget-demo" // それでも autoupdate は出さない
	s := GenerateScoopManifest(in)
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("manifest not valid JSON: %v\n%s", err, s)
	}
	if m["bin"] != "Widget.exe" {
		t.Errorf("app manifest should have top-level bin: %v", m["bin"])
	}
	sc, ok := m["shortcuts"].([]any)
	if !ok || len(sc) != 1 {
		t.Fatalf("app manifest should have one shortcut: %v", m["shortcuts"])
	}
	pair := sc[0].([]any)
	if pair[0] != "Widget.exe" || pair[1] != "Widget" {
		t.Errorf("shortcut wrong: %v", pair)
	}
	a64 := m["architecture"].(map[string]any)["64bit"].(map[string]any)
	if _, hasBin := a64["bin"]; hasBin {
		t.Errorf("app manifest should not set per-arch bin: %v", a64)
	}
	if _, hasAuto := m["autoupdate"]; hasAuto {
		t.Errorf("app manifest should omit autoupdate: %v", m)
	}
}
