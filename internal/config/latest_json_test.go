package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func latestConfig() Config {
	return Config{
		Project: "widget",
		Github:  "acme/widget-demo",
	}
}

func latestAssets() []LatestAsset {
	return []LatestAsset{
		{OS: "darwin", Arch: "arm64", Name: "widget_1.2.3_darwin_arm64.tar.gz"},
		{OS: "linux", Arch: "amd64", Name: "widget_1.2.3_linux_amd64.tar.gz"},
		// GoReleaser の Linux Package は Kind 空で archive と同じ os/arch を持つ。
		// 拡張子で種別を割れないと deb/rpm が archive のキーを潰してしまう(#9 と同型)。
		{OS: "linux", Arch: "amd64", Name: "widget_1.2.3_linux_amd64.deb"},
		{OS: "linux", Arch: "amd64", Name: "widget_1.2.3_linux_amd64.rpm"},
		{OS: "windows", Arch: "amd64", Name: "widget_1.2.3_windows_amd64.zip"},
	}
}

func TestGenerateLatestJSON(t *testing.T) {
	content, ok := GenerateLatestJSON(latestConfig(), "1.2.3", latestAssets())
	if !ok {
		t.Fatal("GenerateLatestJSON ok=false for resolved github")
	}
	var doc struct {
		Version  string            `json:"version"`
		NotesURL string            `json:"notes_url"`
		Assets   map[string]string `json:"assets"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("latest.json is not valid JSON: %v\n%s", err, content)
	}
	if doc.Version != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", doc.Version)
	}
	if doc.NotesURL != "https://github.com/acme/widget-demo/releases/tag/v1.2.3" {
		t.Errorf("notes_url = %q", doc.NotesURL)
	}
	base := "https://github.com/acme/widget-demo/releases/download/v1.2.3/"
	want := map[string]string{
		"macos-arm64":   base + "widget_1.2.3_darwin_arm64.tar.gz",
		"linux-x64":     base + "widget_1.2.3_linux_amd64.tar.gz",
		"linux-x64-deb": base + "widget_1.2.3_linux_amd64.deb",
		"linux-x64-rpm": base + "widget_1.2.3_linux_amd64.rpm",
		"windows-x64":   base + "widget_1.2.3_windows_amd64.zip",
	}
	if len(doc.Assets) != len(want) {
		t.Errorf("assets count = %d, want %d\n%v", len(doc.Assets), len(want), doc.Assets)
	}
	for k, v := range want {
		if doc.Assets[k] != v {
			t.Errorf("assets[%q] = %q, want %q", k, doc.Assets[k], v)
		}
	}
}

// github(owner/repo)未解決なら URL を組めないので ok=false。
func TestGenerateLatestJSONUnresolvedGithub(t *testing.T) {
	cfg := latestConfig()
	cfg.Github = ""
	if _, ok := GenerateLatestJSON(cfg, "1.2.3", latestAssets()); ok {
		t.Error("ok=true with empty github, want false")
	}
}

// 種別を割れない資産(未知拡張子)や os/arch 欠落は載せない。
func TestGenerateLatestJSONSkipsUnkeyable(t *testing.T) {
	assets := []LatestAsset{
		{OS: "linux", Arch: "amd64", Name: "widget_1.2.3_linux_amd64.tar.gz"},
		{OS: "linux", Arch: "amd64", Name: "checksums.txt"}, // 未知拡張子
		{OS: "", Arch: "amd64", Name: "orphan.tar.gz"},      // os 欠落
	}
	content, ok := GenerateLatestJSON(latestConfig(), "1.2.3", assets)
	if !ok {
		t.Fatal("ok=false")
	}
	var doc struct {
		Assets map[string]string `json:"assets"`
	}
	_ = json.Unmarshal([]byte(content), &doc)
	if len(doc.Assets) != 1 {
		t.Errorf("assets = %v, want only the tar.gz", doc.Assets)
	}
}

// 配布元の宣言(extra)は逐語で載る。wharfy は型も意味も触らない(D-236)。
func TestGenerateLatestJSONCarriesExtraVerbatim(t *testing.T) {
	cfg := latestConfig()
	cfg.LatestExtra = map[string]any{
		"store_format":    5,
		"min_app_version": "1.4.0",
		"sunset":          map[string]any{"version": "2.0", "drops_below": 5},
	}
	content, ok := GenerateLatestJSON(cfg, "1.2.3", latestAssets())
	if !ok {
		t.Fatal("ok=false")
	}
	var doc struct {
		Extra map[string]any `json:"extra"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("latest.json is not valid JSON: %v\n%s", err, content)
	}
	if doc.Extra["store_format"] != float64(5) { // 整数は数値のまま(文字列にしない)
		t.Errorf("store_format = %#v, want the number 5", doc.Extra["store_format"])
	}
	if doc.Extra["min_app_version"] != "1.4.0" {
		t.Errorf("min_app_version = %#v", doc.Extra["min_app_version"])
	}
	nested, ok := doc.Extra["sunset"].(map[string]any)
	if !ok || nested["version"] != "2.0" {
		t.Errorf("a nested declaration must survive: %#v", doc.Extra["sunset"])
	}
}

// 宣言が無ければ extra は出ない(書いていない配布者の latest.json は 1 バイトも変わらない)。
func TestGenerateLatestJSONOmitsEmptyExtra(t *testing.T) {
	content, ok := GenerateLatestJSON(latestConfig(), "1.2.3", latestAssets())
	if !ok {
		t.Fatal("ok=false")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatal(err)
	}
	if _, found := doc["extra"]; found {
		t.Errorf("extra must be absent when nothing was declared:\n%s", content)
	}
}

func TestWriteLatestJSONNonDestructive(t *testing.T) {
	root := t.TempDir()
	path, err := WriteLatestJSON(root, "{}\n")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, ".wharfy", "latest.json") {
		t.Errorf("path = %q", path)
	}
	if _, err := os.Stat(filepath.Join(root, "latest.json")); !os.IsNotExist(err) {
		t.Error("must not write latest.json at repo root")
	}
}
