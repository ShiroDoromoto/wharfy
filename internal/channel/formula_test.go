package channel

import (
	"strings"
	"testing"
)

func sampleInput() FormulaInput {
	return FormulaInput{
		Project:     "demo",
		Description: "a demo tool",
		Homepage:    "https://github.com/acme/demo",
		License:     "MIT",
		Version:     "1.2.3",
		Archives: []ArchiveRef{
			{OS: "darwin", Arch: "arm64", URL: "https://x/demo_1.2.3_darwin_arm64.tar.gz", SHA256: "aa"},
			{OS: "darwin", Arch: "amd64", URL: "https://x/demo_1.2.3_darwin_amd64.tar.gz", SHA256: "bb"},
			{OS: "linux", Arch: "amd64", URL: "https://x/demo_1.2.3_linux_amd64.tar.gz", SHA256: "cc"},
			{OS: "linux", Arch: "arm64", URL: "https://x/demo_1.2.3_linux_arm64.tar.gz", SHA256: "dd"},
		},
	}
}

func TestGenerateFormulaStructure(t *testing.T) {
	got := GenerateFormula(sampleInput())
	for _, want := range []string{
		"class Demo < Formula",
		`desc "a demo tool"`,
		`homepage "https://github.com/acme/demo"`,
		`version "1.2.3"`,
		`license "MIT"`,
		"on_macos do", "on_linux do", "on_arm do", "on_intel do",
		`url "https://x/demo_1.2.3_darwin_arm64.tar.gz"`,
		`sha256 "aa"`,
		`bin.install "demo"`,
		`assert_match "1.2.3", shell_output("#{bin}/demo version")`,
		"end\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formula missing %q\n---\n%s", want, got)
		}
	}
}

func TestGenerateFormulaOnlyAvailableOS(t *testing.T) {
	in := sampleInput()
	in.Archives = []ArchiveRef{{OS: "linux", Arch: "amd64", URL: "u", SHA256: "x"}}
	got := GenerateFormula(in)
	if strings.Contains(got, "on_macos") {
		t.Errorf("no darwin archive → should omit on_macos:\n%s", got)
	}
	if !strings.Contains(got, "on_linux") || strings.Contains(got, "on_arm") {
		t.Errorf("linux/amd64 only → on_linux with on_intel, no on_arm:\n%s", got)
	}
}

func TestFormulaVersion(t *testing.T) {
	content := GenerateFormula(sampleInput())
	if v := FormulaVersion(content); v != "1.2.3" {
		t.Errorf("FormulaVersion = %q, want 1.2.3", v)
	}
	if v := FormulaVersion("no version here"); v != "" {
		t.Errorf("FormulaVersion of junk = %q, want empty", v)
	}
}

func TestClassName(t *testing.T) {
	cases := map[string]string{
		"wharfy": "Wharfy", "my-tool": "MyTool", "a_b_c": "ABC", "demo": "Demo",
	}
	for in, want := range cases {
		if got := className(in); got != want {
			t.Errorf("className(%q) = %q, want %q", in, got, want)
		}
	}
}

// 同一入力で生成は決定的(golden/diff の安定の前提)。
func TestGenerateFormulaDeterministic(t *testing.T) {
	a := GenerateFormula(sampleInput())
	b := GenerateFormula(sampleInput())
	if a != b {
		t.Error("formula generation is not deterministic")
	}
}
