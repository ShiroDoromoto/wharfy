package config

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func deprecateResolver() *Resolver {
	return stubResolver("https://github.com/acme/mytool.git", []string{"./cmd/mytool"}, "github.com/acme/mytool")
}

func channelByName(cfg Config, name string) ResolvedChannel {
	for _, ch := range cfg.Channels {
		if ch.Name == name {
			return ch
		}
	}
	return ResolvedChannel{}
}

// 畳んでも channels からは外さない。宣言は該当チャネルに載る(D-3)。
func TestResolveDeprecation(t *testing.T) {
	cfg, err := deprecateResolver().Resolve(File{
		Channels: []string{"homebrew", "script"},
		Deprecate: map[string]*DeprecateInput{
			"script": {Since: "1.4.0", Message: "script チャネルは 1.4.0 で畳みます。"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := channelByName(cfg, "script")
	if script.Deprecated == nil {
		t.Fatal("script must carry its deprecation")
	}
	if script.Deprecated.Since != "1.4.0" {
		t.Errorf("since = %q", script.Deprecated.Since)
	}
	// 文面は逐語。wharfy は言い換えない。
	if script.Deprecated.Message != "script チャネルは 1.4.0 で畳みます。" {
		t.Errorf("message must be carried verbatim, got %q", script.Deprecated.Message)
	}
	// ship の既定は true。畳むと決めた瞬間に入手経路が切れると事故になる。
	if !script.Deprecated.Ship {
		t.Error("ship must default to true")
	}
	// script は告知を載せる欄(実行時 note)を持つ。
	if !script.Deprecated.NoticeSurface {
		t.Error("script must have a notice surface")
	}
	// 宣言していないチャネルには何も載らない。
	if channelByName(cfg, "homebrew").Deprecated != nil {
		t.Error("homebrew was not deprecated; it must carry nothing")
	}
	if len(cfg.OrphanDeprecations) != 0 {
		t.Errorf("no orphans expected, got %v", cfg.OrphanDeprecations)
	}
}

func TestResolveDeprecationShipFalse(t *testing.T) {
	no := false
	cfg, _ := deprecateResolver().Resolve(File{
		Channels:  []string{"script"},
		Deprecate: map[string]*DeprecateInput{"script": {Ship: &no}},
	})
	if channelByName(cfg, "script").Deprecated.Ship {
		t.Error("ship: false must be honored")
	}
}

// 告知を載せる欄が無いチャネルは、そう名乗る。黙って落とすと配布者が気づけない。
func TestResolveDeprecationNoNoticeSurface(t *testing.T) {
	cfg, _ := deprecateResolver().Resolve(File{
		Channels:  []string{"goinstall", "container", "script"},
		Deprecate: map[string]*DeprecateInput{"goinstall": {}, "container": {}, "script": {}},
	})
	for _, name := range []string{"goinstall", "container"} {
		if channelByName(cfg, name).Deprecated.NoticeSurface {
			t.Errorf("%s has no place to carry a notice; it must say so", name)
		}
	}
	if !channelByName(cfg, "script").Deprecated.NoticeSurface {
		t.Error("script has a notice surface")
	}
}

// channels に無いチャネルへの宣言は宙に浮く。禁じないが、黙ってもいない。
func TestResolveDeprecationOrphan(t *testing.T) {
	cfg, _ := deprecateResolver().Resolve(File{
		Channels:  []string{"homebrew"},
		Deprecate: map[string]*DeprecateInput{"script": {}, "aur": {}},
	})
	if want := []string{"aur", "script"}; !reflect.DeepEqual(cfg.OrphanDeprecations, want) {
		t.Errorf("orphans = %v, want %v (sorted, deterministic)", cfg.OrphanDeprecations, want)
	}
}

// latest.json は「すでに入れた人」に届く唯一の経路(caveats は install 時にしか出ない)。
func TestLatestJSONCarriesDeprecations(t *testing.T) {
	cfg, _ := deprecateResolver().Resolve(File{
		Channels:  []string{"script", "homebrew"},
		Deprecate: map[string]*DeprecateInput{"script": {Since: "1.4.0", Message: "使うのをやめてください"}},
	})
	content, ok := GenerateLatestJSON(cfg, "1.4.0", []LatestAsset{{OS: "darwin", Arch: "arm64", Name: "x_1.4.0_darwin_arm64.tar.gz"}})
	if !ok {
		t.Fatal("latest.json must generate")
	}
	var doc struct {
		Deprecations map[string]struct {
			Since   string `json:"since"`
			Ship    bool   `json:"ship"`
			Message string `json:"message"`
		} `json:"deprecations"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatal(err)
	}
	d, found := doc.Deprecations["script"]
	if !found {
		t.Fatalf("latest.json must carry the script deprecation:\n%s", content)
	}
	if d.Since != "1.4.0" || d.Message != "使うのをやめてください" || !d.Ship {
		t.Errorf("deprecation = %+v", d)
	}
	if _, found := doc.Deprecations["homebrew"]; found {
		t.Error("homebrew was not deprecated")
	}
}

// 受け入れ条件: deprecate を書かなければ生成物は 1 バイトも変わらない(純粋な追加)。
func TestNoDeprecationChangesNothing(t *testing.T) {
	cfg, _ := deprecateResolver().Resolve(File{Channels: []string{"script", "releases"}})
	assets := []LatestAsset{{OS: "darwin", Arch: "arm64", Name: "x_1.4.0_darwin_arm64.tar.gz"}}

	latest, _ := GenerateLatestJSON(cfg, "1.4.0", assets)
	if strings.Contains(latest, "deprecations") {
		t.Errorf("latest.json must not mention deprecations when none are declared:\n%s", latest)
	}
	// 解決済み設定にも痕跡を残さない。
	b, _ := json.Marshal(cfg)
	if strings.Contains(string(b), "deprecated") || strings.Contains(string(b), "orphan_deprecations") {
		t.Errorf("resolved config must be unchanged:\n%s", b)
	}
	// 生成物(install.sh / install.ps1)も同様。
	for name, got := range map[string]string{
		"install.sh":  GenerateInstallScript(cfg, "1.4.0"),
		"install.ps1": GenerateInstallPS1(cfg, "1.4.0"),
	} {
		if strings.Contains(strings.ToLower(got), "deprecat") {
			t.Errorf("%s must not mention deprecation when none is declared", name)
		}
	}
}

// 生成先の構文を壊しにくる文面。
const hostileNotice = "畳みます。 It's over.\n$HOME and `whoami` and $(id -u)\nplain ascii line"

func scriptCfgWithNotice(notice string) Config {
	return Config{Project: "mytool", Github: "acme/mytool",
		Channels: []ResolvedChannel{{Name: "script", Kind: "owned",
			Deprecated: &Deprecation{Since: "1.4.0", Ship: true, Message: notice, NoticeSurface: true}}}}
}

// install.sh は各行をシングルクォートで括る。$var も `cmd` も評価されない。
func TestInstallScriptCarriesNotice(t *testing.T) {
	got := GenerateInstallScript(scriptCfgWithNotice(hostileNotice), "1.4.0")
	for _, want := range []string{
		`echo '畳みます。 It'\''s over.' >&2`,
		"echo '$HOME and `whoami` and $(id -u)' >&2",
		`echo 'plain ascii line' >&2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("install.sh missing %q\n---\n%s", want, got)
		}
	}
}

// install.ps1 の非 ASCII 行は base64 にする。Windows 同梱の PowerShell 5.1 は BOM の無い
// スクリプトを ANSI で読むので、日本語を直接書くと読み込んだ時点で壊れる(実機で確認済み)。
func TestInstallPS1EncodesNonASCIINotice(t *testing.T) {
	got := GenerateInstallPS1(scriptCfgWithNotice(hostileNotice), "1.4.0")

	// 非 ASCII の行はそのまま埋め込まれていない。
	if strings.Contains(got, "畳みます") {
		t.Error("non-ASCII must not be written as a literal: PowerShell 5.1 reads it as ANSI and corrupts it")
	}
	// base64 は "畳みます。 It's over." の UTF-8。
	want := base64.StdEncoding.EncodeToString([]byte("畳みます。 It's over."))
	if !strings.Contains(got, "FromBase64String('"+want+"')") {
		t.Errorf("the non-ASCII line must be base64-encoded UTF-8\n---\n%s", got)
	}
	// ASCII の行はそのまま(読める形を保つ)。
	if !strings.Contains(got, `Write-Host 'plain ascii line'`) {
		t.Error("ASCII lines stay literal")
	}
	if !strings.Contains(got, "Write-Host '$HOME and `whoami` and $(id -u)'") {
		t.Error("single-quoted, so PowerShell does not expand $HOME")
	}

	// 告知そのものは ASCII だけで書かれている(コメント行の em dash は実行されないので除く)。
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && !isASCII(line) {
			t.Errorf("executable line is not ASCII: %q", line)
		}
	}
}

// アポストロフィは PowerShell では 2 つ重ねて escape する。
func TestInstallPS1EscapesApostrophe(t *testing.T) {
	got := GenerateInstallPS1(scriptCfgWithNotice("it's fine"), "1.4.0")
	if !strings.Contains(got, `Write-Host 'it''s fine'`) {
		t.Errorf("apostrophe must be doubled\n%s", got)
	}
}

// apt と rpm は別チャネル。片方だけ畳んだ告知が、もう片方のパッケージに載ってはいけない。
func TestGoReleaserSplitsNFPMWhenOnlyOneChannelDeprecated(t *testing.T) {
	cfg := Config{Project: "mytool", Github: "acme/mytool", Main: "./cmd/mytool",
		Build: &Build{GOOS: DefaultGOOS, GOARCH: DefaultGOARCH},
		Channels: []ResolvedChannel{
			{Name: "apt", Kind: "owned", Deprecated: &Deprecation{Ship: true, Message: "apt is going away", NoticeSurface: true}},
			{Name: "rpm", Kind: "owned"},
		}}
	data, err := GenerateGoReleaser(cfg, File{Description: "a tool"})
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		NFPMs []struct {
			ID          string   `yaml:"id"`
			Formats     []string `yaml:"formats"`
			Description string   `yaml:"description"`
		} `yaml:"nfpms"`
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.NFPMs) != 2 {
		t.Fatalf("apt only → nfpms must split per format, got %d entries", len(m.NFPMs))
	}
	for _, e := range m.NFPMs {
		isDeb := len(e.Formats) == 1 && e.Formats[0] == "deb"
		if isDeb != strings.Contains(e.Description, "apt is going away") {
			t.Errorf("the notice must land on deb only: formats=%v desc=%q", e.Formats, e.Description)
		}
	}
}

// 畳んでいなければ nfpms は従来どおり 1 エントリ(生成物は 1 バイトも変わらない)。
func TestGoReleaserKeepsSingleNFPMWhenUndeclared(t *testing.T) {
	cfg := Config{Project: "mytool", Github: "acme/mytool", Main: "./cmd/mytool",
		Build:    &Build{GOOS: DefaultGOOS, GOARCH: DefaultGOARCH},
		Channels: []ResolvedChannel{{Name: "apt", Kind: "owned"}, {Name: "rpm", Kind: "owned"}}}
	data, _ := GenerateGoReleaser(cfg, File{Description: "a tool"})
	var m struct {
		NFPMs []struct {
			Formats []string `yaml:"formats"`
		} `yaml:"nfpms"`
	}
	_ = yaml.Unmarshal(data, &m)
	if len(m.NFPMs) != 1 || len(m.NFPMs[0].Formats) != 2 {
		t.Errorf("undeclared → one entry with both formats, got %+v", m.NFPMs)
	}
}
