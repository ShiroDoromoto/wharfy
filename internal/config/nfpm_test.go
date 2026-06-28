package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func pkgConfig(channels ...string) Config {
	chs := make([]ResolvedChannel, 0, len(channels))
	for _, c := range channels {
		chs = append(chs, ResolvedChannel{Name: c, Kind: "owned"})
	}
	return Config{
		Project: "widget", Main: "./cmd/widget", Github: "acme/widget-demo", License: "MIT",
		Channels: chs, Build: &Build{GOOS: DefaultGOOS, GOARCH: DefaultGOARCH},
	}
}

func nfpmFormats(t *testing.T, cfg Config) []string {
	t.Helper()
	data, err := GenerateGoReleaser(cfg, File{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	nf, ok := m["nfpms"].([]any)
	if !ok {
		return nil
	}
	entry := nf[0].(map[string]any)
	if entry["maintainer"] == "" || entry["maintainer"] == nil {
		t.Errorf("nfpm maintainer required (deb): %v", entry["maintainer"])
	}
	var out []string
	for _, f := range entry["formats"].([]any) {
		out = append(out, f.(string))
	}
	return out
}

func TestNFPMFormats(t *testing.T) {
	if f := nfpmFormats(t, pkgConfig("homebrew")); f != nil {
		t.Errorf("no apt/rpm → no nfpms, got %v", f)
	}
	if f := nfpmFormats(t, pkgConfig("apt")); len(f) != 1 || f[0] != "deb" {
		t.Errorf("apt → [deb], got %v", f)
	}
	if f := nfpmFormats(t, pkgConfig("rpm")); len(f) != 1 || f[0] != "rpm" {
		t.Errorf("rpm → [rpm], got %v", f)
	}
	if f := nfpmFormats(t, pkgConfig("apt", "rpm")); len(f) != 2 || f[0] != "deb" || f[1] != "rpm" {
		t.Errorf("apt+rpm → [deb, rpm], got %v", f)
	}
}

// nfpmOverridesOf は生成 YAML から nfpms[0].overrides を取り出す(無ければ nil)。
func nfpmOverridesOf(t *testing.T, cfg Config, in File) map[string]any {
	t.Helper()
	data, err := GenerateGoReleaser(cfg, in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	nf, ok := m["nfpms"].([]any)
	if !ok || len(nf) == 0 {
		return nil
	}
	ov, _ := nf[0].(map[string]any)["overrides"].(map[string]any)
	return ov
}

func strSlice(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.(string))
	}
	return out
}

func TestNFPMOverrides(t *testing.T) {
	cfg := pkgConfig("apt", "rpm")
	in := File{
		// 依存名はディストロで違うので apt と rpm を別宣言。入力は非ソート。
		Apt: &RepoInput{Depends: []string{"git", "ca-certificates"}, Recommends: []string{"bash-completion"}},
		Rpm: &RepoInput{Depends: []string{"git-core"}},
	}
	ov := nfpmOverridesOf(t, cfg, in)
	if ov == nil {
		t.Fatal("expected overrides for apt+rpm with deps")
	}
	deb, _ := ov["deb"].(map[string]any)
	if got := strSlice(deb["depends"]); len(got) != 2 || got[0] != "ca-certificates" || got[1] != "git" {
		t.Errorf("deb.depends should be sorted [ca-certificates git], got %v", deb["depends"])
	}
	if got := strSlice(deb["recommends"]); len(got) != 1 || got[0] != "bash-completion" {
		t.Errorf("deb.recommends = %v", deb["recommends"])
	}
	rpm, _ := ov["rpm"].(map[string]any)
	if got := strSlice(rpm["depends"]); len(got) != 1 || got[0] != "git-core" {
		t.Errorf("rpm.depends = %v (format isolation: rpm must not inherit apt deps)", rpm["depends"])
	}
	if _, ok := rpm["recommends"]; ok {
		t.Errorf("rpm declared no recommends → key must be omitted, got %v", rpm["recommends"])
	}
}

func TestNFPMOverridesOmitted(t *testing.T) {
	// 依存無し → overrides を出さない(後方互換)。
	if ov := nfpmOverridesOf(t, pkgConfig("apt", "rpm"), File{}); ov != nil {
		t.Errorf("no deps → no overrides, got %v", ov)
	}
	// apt のみ宣言 → deb override だけ。rpm チャネル無効なら rpm 宣言は無視。
	cfg := pkgConfig("apt")
	in := File{
		Apt: &RepoInput{Depends: []string{"git"}},
		Rpm: &RepoInput{Depends: []string{"should-not-appear"}},
	}
	ov := nfpmOverridesOf(t, cfg, in)
	if _, ok := ov["deb"]; !ok {
		t.Errorf("apt deps → deb override expected, got %v", ov)
	}
	if _, ok := ov["rpm"]; ok {
		t.Errorf("rpm channel disabled → no rpm override, got %v", ov)
	}
}

func TestNFPMOverrides_RuntimeDepsMerge(t *testing.T) {
	cfg := pkgConfig("apt", "rpm")
	in := File{
		Apt: &RepoInput{Depends: []string{"git"}},
		RuntimeDeps: []RuntimeDep{
			{Name: "ffmpeg", Min: "6.0"},          // 必須 → 両 format の depends に版付き
			{Name: "fzf", Required: boolp(false)}, // 任意 → recommends
		},
	}
	ov := nfpmOverridesOf(t, cfg, in)
	deb, _ := ov["deb"].(map[string]any)
	if got := strSlice(deb["depends"]); len(got) != 2 || got[0] != "ffmpeg (>= 6.0)" || got[1] != "git" {
		t.Errorf("deb.depends = %v (want [ffmpeg (>= 6.0) git])", deb["depends"])
	}
	if got := strSlice(deb["recommends"]); len(got) != 1 || got[0] != "fzf" {
		t.Errorf("deb.recommends = %v (want [fzf])", deb["recommends"])
	}
	rpm, _ := ov["rpm"].(map[string]any)
	if got := strSlice(rpm["depends"]); len(got) != 1 || got[0] != "ffmpeg >= 6.0" {
		t.Errorf("rpm.depends = %v (want [ffmpeg >= 6.0]; rpm version syntax)", rpm["depends"])
	}
}

func TestResolveAptRpmTarget(t *testing.T) {
	r := stubResolver("https://github.com/acme/widget-demo.git", []string{"./cmd/widget"}, "github.com/acme/widget-demo")
	cfg, err := r.Resolve(File{
		Project:  "widget",
		Channels: []string{"apt", "rpm"},
		Apt:      &RepoInput{Repo: "https://pkg.example.com/acme/repo"},
		Rpm:      &RepoInput{Repo: "https://pkg.example.com/acme/rpm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := targetOf(cfg, "apt"); got != "https://pkg.example.com/acme/repo" {
		t.Errorf("apt target = %q", got)
	}
	if got := targetOf(cfg, "rpm"); got != "https://pkg.example.com/acme/rpm" {
		t.Errorf("rpm target = %q", got)
	}
}
