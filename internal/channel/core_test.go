package channel

import "testing"

// core_test.go — 上流 core formula の版の読み取り(verify の照合基点・#1531)。

// core の formula は version stanza を持たず、Homebrew が url のタグから版を推す。
// url から読めないと、健全な配布を「まだ merge されていない」と誤診する。
func TestCoreFormulaVersionReadsTheURLTag(t *testing.T) {
	got := CoreFormulaVersion(GenerateCoreFormula(CoreFormulaInput{
		Project: "widget", Version: "1.2.3",
		SourceURL: "https://github.com/acme/widget/archive/refs/tags/v1.2.3.tar.gz",
		SourceSHA: "abc",
	}))
	if got != "1.2.3" {
		t.Fatalf("version should come from the url tag: %q", got)
	}
}

// version stanza を持つ formula(上流の書き手が明示した)は、そちらが正。
func TestCoreFormulaVersionPrefersTheVersionStanza(t *testing.T) {
	src := "class Widget < Formula\n" +
		"  url \"https://github.com/acme/widget/archive/refs/tags/v1.2.3.tar.gz\"\n" +
		"  version \"1.2.4\"\nend\n"
	if got := CoreFormulaVersion(src); got != "1.2.4" {
		t.Fatalf("an explicit version stanza wins: %q", got)
	}
}

// どちらからも読めない formula は空を返す(黙って版を捏造しない)。
func TestCoreFormulaVersionUnreadable(t *testing.T) {
	if got := CoreFormulaVersion("class Widget < Formula\n  url \"https://example.com/widget.tar.gz\"\nend\n"); got != "" {
		t.Fatalf("an unreadable formula must not be given a version: %q", got)
	}
}
