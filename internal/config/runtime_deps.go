package config

// runtime_deps.go — 横断 runtime_deps を各 owned パッケージチャネルの依存記法へ射影する
// (設計 design-runtime-deps-first-class-and-gated-distribution.md)。
//
// 射影の要点:
//   - 同名なら 1 宣言で全チャネルに出す。名前が割れる/凝った記法は As で per-channel 逐語上書き。
//   - Min はチャネルの表現力に合わせる: apt `x (>= m)` / rpm `x >= m` / aur `x>=m`、
//     homebrew・scoop は名前のみに縮退(仕様・エラーにしない)。
//   - Required: apt/rpm/aur は必須/任意で別バケツへ。homebrew/scoop は必須のみ出す
//     (As で明示した依存は単一バケツゆえ常に出す)。
//   - 既存の per-channel フィールド(homebrew.dependencies 等)と和集合でマージし重複を畳む。
//
// 凍結契約 Config は広げない: 依存は Description と同じ「生成専用の非公開入力」経路(File)に乗せる。

import (
	"fmt"
	"sort"
)

// チャネルキー(As のキー・射影の分岐に使う)。
const (
	chHomebrew = "homebrew"
	chScoop    = "scoop"
	chApt      = "apt"
	chRpm      = "rpm"
	chAur      = "aur"
)

// isRequired は Required 未指定(nil)を true とみなす。
func (d RuntimeDep) isRequired() bool { return d.Required == nil || *d.Required }

// versioned は Min をチャネルの制約記法に合わせた依存トークンを返す(As 不使用時)。
// Min 空、または制約を表現できないチャネル(homebrew/scoop)では名前のみ。
func (d RuntimeDep) versioned(channel string) string {
	if d.Min == "" {
		return d.Name
	}
	switch channel {
	case chApt:
		return fmt.Sprintf("%s (>= %s)", d.Name, d.Min)
	case chRpm:
		return fmt.Sprintf("%s >= %s", d.Name, d.Min)
	case chAur:
		return fmt.Sprintf("%s>=%s", d.Name, d.Min)
	default: // homebrew / scoop — 名前のみに縮退
		return d.Name
	}
}

// runtimeDepsFor は 1 チャネルへの射影を必須/任意の 2 バケツで返す。
// homebrew/scoop は単一バケツ(required のみ)だが、As で明示した依存は required 扱いで常に出す。
func runtimeDepsFor(deps []RuntimeDep, channel string) (required, optional []string) {
	twoBucket := channel == chApt || channel == chRpm || channel == chAur
	for _, d := range deps {
		override, hasOverride := d.As[channel]
		if d.Name == "" && !hasOverride {
			continue
		}
		tok := override // As は逐語(verbatim)
		if !hasOverride {
			tok = d.versioned(channel)
		}
		if tok == "" {
			continue
		}
		switch {
		case d.isRequired():
			required = append(required, tok)
		case twoBucket:
			optional = append(optional, tok)
		case hasOverride:
			// 単一バケツ(homebrew/scoop)で任意指定でも、明示上書きは出す。
			required = append(required, tok)
		default:
			// homebrew/scoop の任意依存(上書き無し)は出さない。
		}
	}
	return required, optional
}

// HomebrewDeps は homebrew formula の depends_on に出す依存(per-channel 宣言 ∪ 横断必須)。
func HomebrewDeps(in File) []string {
	var deps []string
	if in.Homebrew != nil {
		deps = append(deps, in.Homebrew.Dependencies...)
	}
	req, _ := runtimeDepsFor(in.RuntimeDeps, chHomebrew)
	return dedupeSorted(append(deps, req...))
}

// ScoopDeps は scoop manifest の depends に出す依存(per-channel 宣言 ∪ 横断必須)。
func ScoopDeps(in File) []string {
	var deps []string
	if in.Scoop != nil {
		deps = append(deps, in.Scoop.Dependencies...)
	}
	req, _ := runtimeDepsFor(in.RuntimeDeps, chScoop)
	return dedupeSorted(append(deps, req...))
}

// AurDeps は aur PKGBUILD の depends / optdepends に出す依存(横断のみ・per-channel 宣言は無し)。
func AurDeps(in File) (depends, optdepends []string) {
	req, opt := runtimeDepsFor(in.RuntimeDeps, chAur)
	return dedupeSorted(req), dedupeSorted(opt)
}

// repoDeps は apt/rpm(nfpm)の 1 フォーマット向けに、per-channel 宣言と横断 runtime_deps を
// マージした Depends/Recommends/Suggests を返す。channel は chApt|chRpm。
func repoDeps(channel string, repo *RepoInput, deps []RuntimeDep) (depends, recommends, suggests []string) {
	var d, r, s []string
	if repo != nil {
		d, r, s = repo.Depends, repo.Recommends, repo.Suggests
	}
	req, opt := runtimeDepsFor(deps, channel)
	depends = dedupeSorted(append(append([]string(nil), d...), req...))
	recommends = dedupeSorted(append(append([]string(nil), r...), opt...))
	suggests = dedupeSorted(s)
	return depends, recommends, suggests
}

// dedupeSorted は重複を畳んで sort したコピーを返す(空なら nil で omitempty に乗せる)。
func dedupeSorted(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}
