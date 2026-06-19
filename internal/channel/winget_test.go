package channel

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func wingetInput() WingetInput {
	return WingetInput{
		Identifier:  "ShiroDoromoto.widget",
		Project:     "widget",
		Version:     "1.2.3",
		License:     "MIT",
		Description: "a widget",
		Homepage:    "https://github.com/ShiroDoromoto/widget-demo",
		Installers: []WingetInstaller{
			{Arch: "amd64", URL: "https://x/widget_1.2.3_windows_amd64.zip", SHA256: "abc123"},
			{Arch: "arm64", URL: "https://x/widget_1.2.3_windows_arm64.zip", SHA256: "def456"},
		},
	}
}

func TestWingetPaths(t *testing.T) {
	in := wingetInput()
	if d := in.ManifestDir(); d != "manifests/s/ShiroDoromoto/widget/1.2.3/" {
		t.Errorf("ManifestDir = %q", d)
	}
	if b := in.BranchName(); b != "wharfy/widget-1.2.3" {
		t.Errorf("BranchName = %q", b)
	}
}

func TestGenerateWingetManifests(t *testing.T) {
	files := GenerateWingetManifests(wingetInput())
	for _, name := range []string{
		"ShiroDoromoto.widget.yaml",
		"ShiroDoromoto.widget.installer.yaml",
		"ShiroDoromoto.widget.locale.en-US.yaml",
	} {
		if _, ok := files[name]; !ok {
			t.Errorf("missing manifest %q (have %v)", name, keysOf(files))
		}
	}

	// installer: amd64→x64、sha は大文字、zip の nested portable。
	var inst map[string]any
	if err := yaml.Unmarshal([]byte(files["ShiroDoromoto.widget.installer.yaml"]), &inst); err != nil {
		t.Fatalf("installer not valid YAML: %v", err)
	}
	installers := inst["Installers"].([]any)
	a0 := installers[0].(map[string]any)
	if a0["Architecture"] != "x64" {
		t.Errorf("amd64 should map to x64: %v", a0["Architecture"])
	}
	if a0["InstallerSha256"] != "ABC123" {
		t.Errorf("sha should be uppercased: %v", a0["InstallerSha256"])
	}
	if a0["NestedInstallerType"] != "portable" {
		t.Errorf("zip should use nested portable: %v", a0)
	}

	// version / locale の必須キー。
	if !strings.Contains(files["ShiroDoromoto.widget.yaml"], "ManifestType: version") {
		t.Errorf("version manifest wrong:\n%s", files["ShiroDoromoto.widget.yaml"])
	}
	if !strings.Contains(files["ShiroDoromoto.widget.locale.en-US.yaml"], "Publisher: ShiroDoromoto") {
		t.Errorf("locale manifest wrong")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
