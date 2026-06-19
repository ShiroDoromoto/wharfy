package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func containerConfig() Config {
	return Config{
		Project: "widget", Main: "./cmd/widget", Github: "acme/widget-demo", License: "MIT",
		Channels: []ResolvedChannel{{Name: "container", Kind: "owned", Target: "ghcr.io/acme/widget"}},
		Build:    &Build{GOOS: DefaultGOOS, GOARCH: DefaultGOARCH},
	}
}

func TestGenerateGoReleaserDockersV2(t *testing.T) {
	data, err := GenerateGoReleaser(containerConfig(), File{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	// 旧形式(deprecated)は出さない。
	if _, ok := m["dockers"]; ok {
		t.Error("must not emit deprecated 'dockers'")
	}
	if _, ok := m["docker_manifests"]; ok {
		t.Error("must not emit deprecated 'docker_manifests'")
	}
	dv2, ok := m["dockers_v2"].([]any)
	if !ok || len(dv2) != 1 {
		t.Fatalf("want 1 dockers_v2 entry, got %v", m["dockers_v2"])
	}
	d := dv2[0].(map[string]any)
	if d["images"].([]any)[0] != "ghcr.io/acme/widget" {
		t.Errorf("images wrong: %v", d["images"])
	}
	if !equalAny(d["tags"], []string{"{{ .Version }}", "latest"}) {
		t.Errorf("tags wrong: %v", d["tags"])
	}
	if !equalAny(d["platforms"], []string{"linux/amd64", "linux/arm64"}) {
		t.Errorf("platforms wrong: %v", d["platforms"])
	}
	if d["dockerfile"] != ".wharfy/Dockerfile" {
		t.Errorf("dockerfile wrong: %v", d["dockerfile"])
	}
}

func TestNoContainerNoDockers(t *testing.T) {
	cfg := containerConfig()
	cfg.Channels = []ResolvedChannel{{Name: "homebrew", Kind: "owned", Target: "acme/homebrew-widget"}}
	data, _ := GenerateGoReleaser(cfg, File{})
	var m map[string]any
	_ = yaml.Unmarshal(data, &m)
	if _, ok := m["dockers_v2"]; ok {
		t.Errorf("no container → no dockers_v2")
	}
}

func TestGenerateDockerfile(t *testing.T) {
	df := GenerateDockerfile(containerConfig())
	for _, want := range []string{"FROM scratch", "COPY widget /widget", `ENTRYPOINT ["/widget"]`} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile missing %q\n%s", want, df)
		}
	}
}

// ghcr のイメージ名は小文字必須。owner/project の大文字を下げる。
func TestResolveContainerTargetLowercased(t *testing.T) {
	r := stubResolver("https://github.com/Acme/Widget-Demo.git", []string{"./cmd/widget"}, "github.com/Acme/Widget-Demo")
	cfg, err := r.Resolve(File{Project: "Widget", Channels: []string{"container"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := targetOf(cfg, "container"); got != "ghcr.io/acme/widget" {
		t.Errorf("container target = %q, want ghcr.io/acme/widget (lowercased)", got)
	}
}
