package config

import (
	"errors"
	"reflect"
	"testing"
)

// stubResolver は外部 I/O を固定値に差し替えた Resolver を作る(末端は差し替え可能・01)。
func stubResolver(origin string, mains []string, module string) *Resolver {
	return &Resolver{
		Root:       "/fake/root",
		OriginURL:  func(string) (string, error) { return origin, nil },
		MainPkgs:   func(string) ([]string, error) { return mains, nil },
		ModulePath: func(string) (string, error) { return module, nil },
	}
}

func TestResolveDefaults(t *testing.T) {
	r := stubResolver("https://github.com/acme/mytool.git", []string{"./cmd/mytool"}, "github.com/acme/mytool")
	cfg, err := r.Resolve(File{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Project != "mytool" {
		t.Errorf("project = %q, want mytool", cfg.Project)
	}
	if cfg.Github != "acme/mytool" {
		t.Errorf("github = %q, want acme/mytool", cfg.Github)
	}
	if cfg.Main != "./cmd/mytool" {
		t.Errorf("main = %q, want ./cmd/mytool", cfg.Main)
	}
	if cfg.Homepage != "https://github.com/acme/mytool" {
		t.Errorf("homepage = %q", cfg.Homepage)
	}
	if got := channelNames(cfg); !reflect.DeepEqual(got, DefaultChannels) {
		t.Errorf("channels = %v, want %v", got, DefaultChannels)
	}
	if tgt := targetOf(cfg, "homebrew"); tgt != "acme/homebrew-mytool" {
		t.Errorf("homebrew target = %q, want acme/homebrew-mytool", tgt)
	}
	if tgt := targetOf(cfg, "goinstall"); tgt != "github.com/acme/mytool" {
		t.Errorf("goinstall target = %q", tgt)
	}
	if cfg.Build == nil || !reflect.DeepEqual(cfg.Build.GOOS, DefaultGOOS) {
		t.Errorf("build goos = %+v, want %v", cfg.Build, DefaultGOOS)
	}
}

func TestResolveExplicitWins(t *testing.T) {
	r := stubResolver("https://github.com/acme/mytool.git", []string{"./cmd/mytool"}, "github.com/acme/mytool")
	in := File{
		Project:  "renamed",
		Github:   "other/repo",
		Main:     "./app",
		Homepage: "https://example.com",
		License:  "MIT",
		Channels: []string{"homebrew", "winget"},
		Homebrew: &HomebrewInput{Tap: "other/homebrew-tools"},
	}
	cfg, err := r.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project != "renamed" || cfg.Github != "other/repo" || cfg.Main != "./app" {
		t.Errorf("explicit values not honored: %+v", cfg)
	}
	if cfg.Homepage != "https://example.com" || cfg.License != "MIT" {
		t.Errorf("homepage/license not honored: %+v", cfg)
	}
	if targetOf(cfg, "homebrew") != "other/homebrew-tools" {
		t.Errorf("explicit tap not honored: %q", targetOf(cfg, "homebrew"))
	}
	if k := kindOf(cfg, "winget"); k != "gated" {
		t.Errorf("winget kind = %q, want gated", k)
	}
}

// releases の発行先は実リポジトリ(github)。project 名と repo 名が違っても github を使う。
func TestResolveReleasesTargetIsGithub(t *testing.T) {
	r := stubResolver("https://github.com/acme/widget-demo.git", []string{"./cmd/widget"}, "github.com/acme/widget-demo")
	cfg, err := r.Resolve(File{Project: "widget", Channels: []string{"homebrew", "releases"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := targetOf(cfg, "releases"); got != "acme/widget-demo" {
		t.Errorf("releases target = %q, want acme/widget-demo (the real repo, not owner/project)", got)
	}
	// homebrew は ADR-8 の <owner>/homebrew-<project> のまま。
	if got := targetOf(cfg, "homebrew"); got != "acme/homebrew-widget" {
		t.Errorf("homebrew target = %q, want acme/homebrew-widget", got)
	}
}

// script.base_url 指定時、script チャネルの Target は <base_url>/install.sh になる。
func TestResolveScriptBaseURL(t *testing.T) {
	r := stubResolver("https://github.com/acme/widget-demo.git", []string{"./cmd/widget"}, "github.com/acme/widget-demo")
	cfg, err := r.Resolve(File{
		Project:  "widget",
		Channels: []string{"script"},
		Script:   &ScriptInput{BaseURL: "https://cdn.example.com/wharfy/"}, // 末尾スラッシュは正規化される
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := targetOf(cfg, "script"); got != "https://cdn.example.com/wharfy/install.sh" {
		t.Errorf("script target = %q, want https://cdn.example.com/wharfy/install.sh", got)
	}
	if got := InstallURL(cfg); got != "https://cdn.example.com/wharfy/install.sh" {
		t.Errorf("InstallURL = %q, want the vanity URL", got)
	}
}

// base_url 未指定なら script Target は空のまま、InstallURL は GitHub Releases latest にフォールバック。
func TestResolveScriptNoBaseURL(t *testing.T) {
	r := stubResolver("https://github.com/acme/widget-demo.git", []string{"./cmd/widget"}, "github.com/acme/widget-demo")
	cfg, err := r.Resolve(File{Project: "widget", Channels: []string{"script"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := targetOf(cfg, "script"); got != "" {
		t.Errorf("script target = %q, want empty (no base_url)", got)
	}
	if got := InstallURL(cfg); got != "https://github.com/acme/widget-demo/releases/latest/download/install.sh" {
		t.Errorf("InstallURL = %q, want GitHub Releases latest", got)
	}
}

func TestResolvePrefersCmdProject(t *testing.T) {
	// ./cmd/<project> が候補にあれば、複数 main でもそれを選ぶ(曖昧扱いしない)。
	r := stubResolver("https://github.com/acme/mytool.git",
		[]string{"./other", "./cmd/mytool"}, "github.com/acme/mytool")
	cfg, err := r.Resolve(File{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Main != "./cmd/mytool" {
		t.Errorf("main = %q, want ./cmd/mytool", cfg.Main)
	}
}

func TestResolveMainAmbiguous(t *testing.T) {
	r := stubResolver("https://github.com/acme/mytool.git",
		[]string{"./cmd/a", "./cmd/b"}, "github.com/acme/mytool")
	cfg, err := r.Resolve(File{})

	var amb *AmbiguousMainError
	if !errors.As(err, &amb) {
		t.Fatalf("expected AmbiguousMainError, got %v", err)
	}
	if !reflect.DeepEqual(amb.Candidates, []string{"./cmd/a", "./cmd/b"}) {
		t.Errorf("candidates = %v", amb.Candidates)
	}
	// 部分解決した実効設定は依然 valid(project + channels を持つ)。
	if cfg.Project != "mytool" || len(cfg.Channels) == 0 {
		t.Errorf("partial config not usable: %+v", cfg)
	}
	if cfg.Main != "" {
		t.Errorf("main should be empty on ambiguity, got %q", cfg.Main)
	}
}

func TestResolveMainZeroCandidates(t *testing.T) {
	r := stubResolver("https://github.com/acme/mytool.git", nil, "github.com/acme/mytool")
	_, err := r.Resolve(File{})
	var amb *AmbiguousMainError
	if !errors.As(err, &amb) || len(amb.Candidates) != 0 {
		t.Fatalf("expected zero-candidate AmbiguousMainError, got %v", err)
	}
}

func TestResolveNoGithub(t *testing.T) {
	// origin が github 以外/取得不可。github/homepage は空、owner 由来の target も空。
	r := stubResolver("git@gitlab.com:acme/mytool.git", []string{"./cmd/mytool"}, "example.com/acme/mytool")
	cfg, err := r.Resolve(File{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Github != "" {
		t.Errorf("github = %q, want empty (non-github remote)", cfg.Github)
	}
	if cfg.Homepage != "" {
		t.Errorf("homepage = %q, want empty", cfg.Homepage)
	}
	if cfg.Project != "mytool" { // module 末尾から推測
		t.Errorf("project = %q, want mytool (module last)", cfg.Project)
	}
	if tgt := targetOf(cfg, "homebrew"); tgt != "" {
		t.Errorf("homebrew target = %q, want empty (owner unknown)", tgt)
	}
}

func TestInferGithub(t *testing.T) {
	cases := []struct {
		url, owner, repo string
		ok               bool
	}{
		{"https://github.com/acme/mytool.git", "acme", "mytool", true},
		{"https://github.com/acme/mytool", "acme", "mytool", true},
		{"git@github.com:acme/mytool.git", "acme", "mytool", true},
		{"ssh://git@github.com/acme/mytool", "acme", "mytool", true},
		{"https://gitlab.com/acme/mytool.git", "", "", false},
		{"not a url", "", "", false},
	}
	for _, c := range cases {
		o, r, ok := inferGithub(c.url)
		if ok != c.ok || o != c.owner || r != c.repo {
			t.Errorf("inferGithub(%q) = (%q,%q,%v), want (%q,%q,%v)", c.url, o, r, ok, c.owner, c.repo, c.ok)
		}
	}
}

func TestSpdxFromText(t *testing.T) {
	cases := map[string]string{
		"                    GNU AFFERO GENERAL PUBLIC LICENSE\n                       Version 3": "AGPL-3.0",
		"Apache License\nVersion 2.0":  "Apache-2.0",
		"MIT License\n\nCopyright (c)": "MIT",
		"some random text":             "",
	}
	for text, want := range cases {
		if got := spdxFromText(text); got != want {
			t.Errorf("spdxFromText(%q) = %q, want %q", text, got, want)
		}
	}
}

// --- helpers ---

func channelNames(cfg Config) []string {
	out := make([]string, 0, len(cfg.Channels))
	for _, c := range cfg.Channels {
		out = append(out, c.Name)
	}
	return out
}

func targetOf(cfg Config, name string) string {
	for _, c := range cfg.Channels {
		if c.Name == name {
			return c.Target
		}
	}
	return ""
}

func kindOf(cfg Config, name string) string {
	for _, c := range cfg.Channels {
		if c.Name == name {
			return c.Kind
		}
	}
	return ""
}

func pushTargetOf(cfg Config, name string) string {
	for _, c := range cfg.Channels {
		if c.Name == name {
			return c.PushTarget
		}
	}
	return ""
}

func TestResolveRepoURLs(t *testing.T) {
	cases := []struct {
		name        string
		channel     string
		in          *RepoInput
		wantDeliver string
		wantPush    string
	}{
		{"nil → both empty", "apt", nil, "", ""},
		{"fury apt → apt+push hosts", "apt", &RepoInput{Provider: "fury", User: "shiro"},
			"https://apt.fury.io/shiro/", "https://push.fury.io/shiro/"},
		{"fury rpm → yum+push hosts", "rpm", &RepoInput{Provider: "fury", User: "shiro"},
			"https://yum.fury.io/shiro/", "https://push.fury.io/shiro/"},
		{"fury without user → falls back to raw (empty)", "apt", &RepoInput{Provider: "fury"}, "", ""},
		{"raw repo only → push falls back to repo", "apt", &RepoInput{Repo: "https://r.example.com/a/"},
			"https://r.example.com/a/", "https://r.example.com/a/"},
		{"raw repo+push → distinct", "apt", &RepoInput{Repo: "https://deliver/", Push: "https://upload/"},
			"https://deliver/", "https://upload/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deliver, push := resolveRepoURLs(tc.channel, tc.in)
			if deliver != tc.wantDeliver || push != tc.wantPush {
				t.Errorf("resolveRepoURLs(%q) = (%q, %q), want (%q, %q)",
					tc.channel, deliver, push, tc.wantDeliver, tc.wantPush)
			}
		})
	}
}

// fury provider 指定が Resolve を通して Target(配信)/PushTarget(アップロード) に反映されること。
func TestResolveChannelsFuryProvider(t *testing.T) {
	r := stubResolver("https://github.com/acme/mytool.git", []string{"./cmd/mytool"}, "github.com/acme/mytool")
	cfg, err := r.Resolve(File{
		Channels: []string{"apt", "rpm"},
		Apt:      &RepoInput{Provider: "fury", User: "shiro"},
		Rpm:      &RepoInput{Provider: "fury", User: "shiro"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := targetOf(cfg, "apt"); got != "https://apt.fury.io/shiro/" {
		t.Errorf("apt target = %q", got)
	}
	if got := pushTargetOf(cfg, "apt"); got != "https://push.fury.io/shiro/" {
		t.Errorf("apt push_target = %q", got)
	}
	if got := targetOf(cfg, "rpm"); got != "https://yum.fury.io/shiro/" {
		t.Errorf("rpm target = %q", got)
	}
	if got := pushTargetOf(cfg, "rpm"); got != "https://push.fury.io/shiro/" {
		t.Errorf("rpm push_target = %q", got)
	}
}
