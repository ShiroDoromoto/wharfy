package channel

import (
	"encoding/json"
	"strings"
	"testing"
)

// hostileNotice は配布者が書きうる、生成先の構文を壊しにくる文面。
// Ruby の式展開・シェル変数・コマンド置換・ヒアドキュメント終端子・引用符を含む。
const hostileNotice = "畳みます。 It's over.\n" +
	"#{system('rm -rf /')} and $HOME and `whoami`\n" +
	"EOS\n" +
	`see "docs" for 100% details`

// 終端子が本文と衝突したら退避する。しないと文字列がそこで切れて formula が壊れる。
func TestHeredocTerminatorAvoidsCollision(t *testing.T) {
	if got := heredocTerminator("plain body", "EOS"); got != "EOS" {
		t.Errorf("no collision → EOS, got %q", got)
	}
	if got := heredocTerminator("a\nEOS\nb", "EOS"); got != "EOS2" {
		t.Errorf("collision → EOS2, got %q", got)
	}
	if got := heredocTerminator("a\nEOS\nEOS2\nb", "EOS"); got != "EOS3" {
		t.Errorf("two collisions → EOS3, got %q", got)
	}
	// 前後に空白があっても「その行だけ」なら衝突とみなす(Ruby の <<~ は行頭空白を無視する)。
	if got := heredocTerminator("a\n  EOS  \nb", "EOS"); got != "EOS2" {
		t.Errorf("indented terminator still collides, got %q", got)
	}
}

// formula の caveats は単一引用ヒアドキュメント。Ruby に式展開させない。
func TestFormulaCarriesNoticeVerbatim(t *testing.T) {
	got := GenerateFormula(FormulaInput{
		Project: "mytool", Version: "1.4.0",
		Archives: []ArchiveRef{{OS: "darwin", Arch: "arm64", URL: "u", SHA256: "s"}},
		Notice:   hostileNotice,
	})
	if !strings.Contains(got, "caveats <<~'EOS2'") {
		t.Errorf("must use a single-quoted heredoc with an escaped terminator:\n%s", got)
	}
	for _, line := range strings.Split(hostileNotice, "\n") {
		if !strings.Contains(got, line) {
			t.Errorf("notice line missing (must be verbatim): %q", line)
		}
	}
	// 告知が無ければ caveats を出さない。
	none := GenerateFormula(FormulaInput{Project: "mytool", Version: "1.4.0",
		Archives: []ArchiveRef{{OS: "darwin", Arch: "arm64", URL: "u", SHA256: "s"}}})
	if strings.Contains(none, "caveats") {
		t.Error("no notice → no caveats")
	}
}

// Ruby の caveats は後勝ちなので、Gatekeeper 案内と告知は 1 つにまとめる。
// 2 つ書くと先に書いたほう(Gatekeeper 案内)が黙って消える。
func TestCaskMergesGatekeeperAndNotice(t *testing.T) {
	got := GenerateCask(CaskInput{
		Token: "mytool", Name: "MyTool", Version: "1.4.0", AppBundle: "MyTool.app",
		Artifacts: []CaskArtifact{{Arch: "arm64", URL: "u", SHA256: "s"}},
		Notarized: false,
		Notice:    "we are winding this down",
	})
	if n := strings.Count(got, "caveats"); n != 1 {
		t.Errorf("exactly one caveats block expected, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "not notarized") {
		t.Error("gatekeeper guidance must survive")
	}
	if !strings.Contains(got, "we are winding this down") {
		t.Error("the distributor's notice must survive")
	}
	// notarized かつ告知なし → caveats を出さない。
	clean := GenerateCask(CaskInput{Token: "t", Version: "1", AppBundle: "a.app", Notarized: true,
		Artifacts: []CaskArtifact{{Arch: "arm64", URL: "u", SHA256: "s"}}})
	if strings.Contains(clean, "caveats") {
		t.Error("notarized and no notice → no caveats")
	}
}

// scoop の notes は JSON なのでエスケープは encoding/json に任せる。1 行 1 要素。
func TestScoopCarriesNotice(t *testing.T) {
	got := GenerateScoopManifest(ScoopInput{
		Project: "mytool", Version: "1.4.0",
		Archives: []ScoopArch{{Arch: "amd64", URL: "u", SHA256: "s"}},
		Notice:   hostileNotice,
	})
	var m struct {
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("manifest must stay valid JSON: %v\n%s", err, got)
	}
	want := strings.Split(hostileNotice, "\n")
	if len(m.Notes) != len(want) {
		t.Fatalf("notes = %v", m.Notes)
	}
	for i := range want {
		if m.Notes[i] != want[i] {
			t.Errorf("notes[%d] = %q, want %q", i, m.Notes[i], want[i])
		}
	}
	none := GenerateScoopManifest(ScoopInput{Project: "mytool", Version: "1.4.0",
		Archives: []ScoopArch{{Arch: "amd64", URL: "u", SHA256: "s"}}})
	if strings.Contains(none, "notes") {
		t.Error("no notice → no notes")
	}
}

// aur は pkgdesc が 1 行なので .install(post_install)で出す。
// 単一引用ヒアドキュメントなので $var も `cmd` もシェルに評価されない。
func TestAurInstallFile(t *testing.T) {
	in := AurInput{Package: "mytool-bin", Project: "mytool", Version: "1.4.0", Notice: hostileNotice}
	files := in.Files()
	install, ok := files["mytool-bin.install"]
	if !ok {
		t.Fatalf("install file expected, files = %v", keys(files))
	}
	if !strings.Contains(install, "cat <<'EOS2'") {
		t.Errorf("single-quoted heredoc with escaped terminator expected:\n%s", install)
	}
	if !strings.Contains(install, "post_upgrade()") {
		t.Error("upgrades must announce too")
	}
	if !strings.Contains(GeneratePKGBUILD(in), "install=mytool-bin.install") {
		t.Error("PKGBUILD must reference the install file")
	}
	if !strings.Contains(GenerateSRCINFO(in), "install = mytool-bin.install") {
		t.Error(".SRCINFO must reference the install file")
	}

	// 告知が無ければ .install を作らず、PKGBUILD も参照しない。
	quiet := AurInput{Package: "mytool-bin", Project: "mytool", Version: "1.4.0"}
	if _, ok := quiet.Files()["mytool-bin.install"]; ok {
		t.Error("no notice → no install file")
	}
	if strings.Contains(GeneratePKGBUILD(quiet), "install=") {
		t.Error("no notice → PKGBUILD must not reference an install file")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
