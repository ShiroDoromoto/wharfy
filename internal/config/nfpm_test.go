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
