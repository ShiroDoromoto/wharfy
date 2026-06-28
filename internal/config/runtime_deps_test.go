package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRuntimeDeps_YAMLParseAndProject(t *testing.T) {
	src := `
runtime_deps:
  - name: ffmpeg
    min: "6.0"
  - name: fzf
    required: false
  - name: rg
    as:
      aur: ripgrep
      homebrew: "node => :recommended"
`
	var in File
	if err := yaml.Unmarshal([]byte(src), &in); err != nil {
		t.Fatal(err)
	}
	if len(in.RuntimeDeps) != 3 {
		t.Fatalf("parsed %d runtime_deps, want 3", len(in.RuntimeDeps))
	}
	if got := HomebrewDeps(in); !reflect.DeepEqual(got, []string{"ffmpeg", "node => :recommended"}) {
		t.Errorf("HomebrewDeps = %v", got)
	}
	dep, opt := AurDeps(in)
	if !reflect.DeepEqual(dep, []string{"ffmpeg>=6.0", "ripgrep"}) {
		t.Errorf("AurDeps depends = %v", dep)
	}
	if !reflect.DeepEqual(opt, []string{"fzf"}) {
		t.Errorf("AurDeps optdepends = %v", opt)
	}
}

func boolp(b bool) *bool { return &b }

func TestHomebrewScoopDeps_UnionAndDegrade(t *testing.T) {
	in := File{
		Homebrew: &HomebrewInput{Dependencies: []string{"git"}},
		Scoop:    &ScoopInput{Dependencies: []string{"git"}},
		RuntimeDeps: []RuntimeDep{
			{Name: "ffmpeg", Min: "6.0"},          // 必須・min は brew/scoop で縮退
			{Name: "fzf", Required: boolp(false)}, // 任意 → brew/scoop では出さない
			{Name: "git"},                         // per-channel と重複 → 畳む
		},
	}
	// min は名前のみに縮退、任意 fzf は出ない、git は重複排除、sort 済み。
	want := []string{"ffmpeg", "git"}
	if got := HomebrewDeps(in); !reflect.DeepEqual(got, want) {
		t.Errorf("HomebrewDeps = %v, want %v", got, want)
	}
	if got := ScoopDeps(in); !reflect.DeepEqual(got, want) {
		t.Errorf("ScoopDeps = %v, want %v", got, want)
	}
}

func TestHomebrewDeps_AsOverrideVerbatim(t *testing.T) {
	in := File{RuntimeDeps: []RuntimeDep{
		// 任意でも as.homebrew を明示したら逐語で出す(単一バケツの逃げ道)。
		{Name: "node", Required: boolp(false), As: map[string]string{"homebrew": "node => :recommended"}},
	}}
	want := []string{"node => :recommended"}
	if got := HomebrewDeps(in); !reflect.DeepEqual(got, want) {
		t.Errorf("HomebrewDeps = %v, want %v", got, want)
	}
	// scoop には as 指定が無く、かつ任意なので出ない。
	if got := ScoopDeps(in); got != nil {
		t.Errorf("ScoopDeps = %v, want nil", got)
	}
}

func TestAurDeps_RequiredOptionalAndVersion(t *testing.T) {
	in := File{RuntimeDeps: []RuntimeDep{
		{Name: "ffmpeg", Min: "6.0"},                          // 必須・版制約
		{Name: "fzf", Required: boolp(false)},                 // 任意 → optdepends
		{Name: "rg", As: map[string]string{"aur": "ripgrep"}}, // 別名(verbatim)
	}}
	dep, opt := AurDeps(in)
	if want := []string{"ffmpeg>=6.0", "ripgrep"}; !reflect.DeepEqual(dep, want) {
		t.Errorf("AurDeps depends = %v, want %v", dep, want)
	}
	if want := []string{"fzf"}; !reflect.DeepEqual(opt, want) {
		t.Errorf("AurDeps optdepends = %v, want %v", opt, want)
	}
}

func TestRepoDeps_RuntimeMergeAndVersionSyntax(t *testing.T) {
	deps := []RuntimeDep{
		{Name: "ffmpeg", Min: "6.0"},          // 必須 → Depends/Requires に版付きで
		{Name: "fzf", Required: boolp(false)}, // 任意 → Recommends
	}
	apt := &RepoInput{Depends: []string{"git"}, Suggests: []string{"bash-completion"}}
	d, r, s := repoDeps(chApt, apt, deps)
	if want := []string{"ffmpeg (>= 6.0)", "git"}; !reflect.DeepEqual(d, want) {
		t.Errorf("apt depends = %v, want %v", d, want)
	}
	if want := []string{"fzf"}; !reflect.DeepEqual(r, want) {
		t.Errorf("apt recommends = %v, want %v", r, want)
	}
	if want := []string{"bash-completion"}; !reflect.DeepEqual(s, want) {
		t.Errorf("apt suggests = %v, want %v", s, want)
	}
	// rpm は別バージョン記法。
	d2, _, _ := repoDeps(chRpm, nil, deps)
	if want := []string{"ffmpeg >= 6.0"}; !reflect.DeepEqual(d2, want) {
		t.Errorf("rpm requires = %v, want %v", d2, want)
	}
}

func TestRuntimeDeps_EmptyNoOutput(t *testing.T) {
	in := File{}
	if got := HomebrewDeps(in); got != nil {
		t.Errorf("empty HomebrewDeps = %v, want nil", got)
	}
	if d, o := AurDeps(in); d != nil || o != nil {
		t.Errorf("empty AurDeps = %v / %v, want nil", d, o)
	}
	if d, r, s := repoDeps(chApt, nil, nil); d != nil || r != nil || s != nil {
		t.Errorf("empty repoDeps = %v/%v/%v, want nil", d, r, s)
	}
}
