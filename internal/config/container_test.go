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

func TestGenerateGoReleaserDockers(t *testing.T) {
	data, err := GenerateGoReleaser(containerConfig(), File{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	dockers, ok := m["dockers"].([]any)
	if !ok || len(dockers) != 2 {
		t.Fatalf("want 2 per-arch dockers, got %v", m["dockers"])
	}
	d0 := dockers[0].(map[string]any)
	if d0["goarch"] != "amd64" || d0["dockerfile"] != ".wharfy/Dockerfile" {
		t.Errorf("docker entry wrong: %+v", d0)
	}
	if img := d0["image_templates"].([]any)[0]; img != "ghcr.io/acme/widget:{{ .Version }}-amd64" {
		t.Errorf("image template wrong: %v", img)
	}
	mans := m["docker_manifests"].([]any)
	if len(mans) != 2 {
		t.Fatalf("want :version and :latest manifests, got %d", len(mans))
	}
	names := []string{mans[0].(map[string]any)["name_template"].(string), mans[1].(map[string]any)["name_template"].(string)}
	if names[0] != "ghcr.io/acme/widget:{{ .Version }}" || names[1] != "ghcr.io/acme/widget:latest" {
		t.Errorf("manifest names wrong: %v", names)
	}
}

func TestNoContainerNoDockers(t *testing.T) {
	cfg := containerConfig()
	cfg.Channels = []ResolvedChannel{{Name: "homebrew", Kind: "owned", Target: "acme/homebrew-widget"}}
	data, _ := GenerateGoReleaser(cfg, File{})
	var m map[string]any
	_ = yaml.Unmarshal(data, &m)
	if _, ok := m["dockers"]; ok {
		t.Errorf("no container → no dockers")
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
