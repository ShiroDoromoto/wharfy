package main

import (
	"fmt"
	"strings"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// deprecate.go — 畳むチャネルの告知を、利用者にも配布者にも見える形にする(D-3)。
//
// wharfy は配布者の文面を作らない。ここが出すのは wharfy 自身の言葉、すなわち
// 「告知が載ったのか、載らなかったのか」だけ。載らなかったことを黙っていると、
// 配布者は告知したつもりのまま利用者を取り残す。

// channelNotice は指定チャネルに宣言された告知の文面を返す(無ければ空)。
// 各チャネルの生成器はこれを自分の注記欄へ逐語で載せる。wharfy は一言も足さない。
func channelNotice(cfg config.Config, channel string) string {
	for _, ch := range cfg.Channels {
		if ch.Name == channel && ch.Deprecated != nil {
			return strings.TrimSpace(ch.Deprecated.Message)
		}
	}
	return ""
}

// deprecationWarnings は畳む宣言から警告を組む。status と publish が同じものを見る。
//
// 出るのは 2 種類:
//   - 告知を載せる欄が無いチャネル(goinstall / container 等) → latest.json 経由でしか届かない
//   - channels に無いチャネルへの宣言 → wharfy はもう触らないので告知の更新が止まっている
func deprecationWarnings(cfg config.Config) []output.Warning {
	var out []output.Warning
	for _, ch := range cfg.Channels {
		if ch.Deprecated == nil || ch.Deprecated.NoticeSurface {
			continue
		}
		out = append(out, output.Warning{
			Code: output.WarnDeprecateNoSurface,
			Message: fmt.Sprintf(
				"%s: deprecated, but this channel has no place to carry your notice — it reaches users only via latest.json",
				ch.Name),
		})
	}
	for _, name := range cfg.OrphanDeprecations {
		out = append(out, output.Warning{
			Code: output.WarnDeprecateOrphan,
			Message: fmt.Sprintf(
				"%s: deprecated but not in channels — wharfy no longer touches it, so the notice is frozen; keep it in channels to keep announcing",
				name),
		})
	}
	return out
}

// --- 凍結(ship:false) ---
//
// 畳んだうえで配るのを止めたチャネルは、最後に配った版のまま据え置く。何を据え置けるかは
// チャネルが持つ生成物で決まるので、扱いは 4 通りに分かれる(freezeMode)。
// 「新版を配らない」はどのチャネルでも守る。「告知だけ更新する」は生成物を作り直せる
// チャネルだけができる。できないチャネルは黙らずに warning で言う(D-3: 告知したつもりを作らない)。

type freezeMode int

const (
	// freezeUnsupported: 凍結できない。wharfy が版を選べるものを持たないチャネル(ゼロ値＝未知も同じ)。
	freezeUnsupported freezeMode = iota
	// freezeManifest: 凍結版の成果物からマニフェストを作り直す。版は据え置き、告知だけ新しくなる。
	freezeManifest
	// freezeScript: release は新版で走るが、install.sh / install.ps1 が入れる版は凍結版のまま。
	freezeScript
	// freezeHold: 何も書かない。既に配った生成物がそのまま残る(告知は latest.json 経由でのみ届く)。
	freezeHold
)

// freezeModes はチャネルごとの凍結の効き方。ここに無いチャネルは凍結できない扱いにする。
var freezeModes = map[string]freezeMode{
	"homebrew": freezeManifest, "cask": freezeManifest, "scoop": freezeManifest, "aur": freezeManifest,
	"script": freezeScript,
	"apt":    freezeHold, "rpm": freezeHold, "container": freezeHold,
	"winget": freezeHold, "homebrew-core": freezeHold,
	// releases: Release そのものが他チャネルの資産置き場なので止められない。
	// goinstall: 梱包ゼロ。go install は module proxy の最新 tag を取るので wharfy に止める術がない。
	"releases": freezeUnsupported, "goinstall": freezeUnsupported,
}

// channelFreeze は 1 チャネルの凍結の解決結果。frozen でないチャネルには作らない(nil)。
type channelFreeze struct {
	Channel   string
	Mode      freezeMode
	Version   string           // 最後に配った版(凍結して配り続ける版)
	Artifacts []build.Artifact // その版の成果物(freezeManifest が生成器に渡す)
	Reason    string           // 降格したときの理由。そのまま warning の本文になる
}

// frozenChannel は そのチャネルが deprecate かつ ship:false か。
func frozenChannel(cfg config.Config, ch string) bool {
	for _, c := range cfg.Channels {
		if c.Name == ch {
			return c.Deprecated != nil && !c.Deprecated.Ship
		}
	}
	return false
}

// resolveFreeze は ship:false のチャネルについて、配り続ける版と成果物を記録から解く。
// 据え置く版が記録に無ければ hold へ降格する — 凍結先が無いのに新版を配るのは、配布者が
// 止めたと思っている経路から新版が漏れることであって、最も避けたい事故。
func resolveFreeze(cfg config.Config, st *state.State, ch string) *channelFreeze {
	if !frozenChannel(cfg, ch) {
		return nil
	}
	mode, known := freezeModes[ch]
	if !known {
		mode = freezeUnsupported
	}
	fz := &channelFreeze{Channel: ch, Mode: mode}
	switch mode {
	case freezeUnsupported:
		fz.Reason = "this channel has nothing wharfy can hold back — it keeps serving whatever the release carries"
		return fz
	case freezeHold:
		fz.Reason = "wharfy writes nothing here; the notice reaches users only via latest.json"
	}

	var rec state.PublishRecord
	if st != nil && st.Publish != nil {
		rec = st.Publish[ch]
	}
	fz.Version, fz.Artifacts = rec.Version, rec.Artifacts

	if fz.Version == "" {
		fz.Mode, fz.Reason = freezeHold, "never published — there is no version to freeze at, so nothing is shipped"
		return fz
	}
	if mode == freezeManifest && len(fz.Artifacts) == 0 {
		fz.Mode = freezeHold
		fz.Reason = "no artifact checksums recorded for " + fz.Version +
			" — the manifest cannot be rebuilt at the frozen version, so the notice stays as published"
	}
	return fz
}

// loadFreeze は単体 publish 用。記録を読んでから resolveFreeze する。
func loadFreeze(root string, cfg config.Config, ch string) *channelFreeze {
	if !frozenChannel(cfg, ch) {
		return nil // 記録を読む前に落とす(凍結していないチャネルが大半)
	}
	st, err := state.Load(root, cfg.Project)
	if err != nil {
		st = nil // 記録が壊れていても凍結の判断は続ける(hold へ降格する)
	}
	return resolveFreeze(cfg, st, ch)
}

// freezeWarning は凍結の効き方を配布者に伝える。凍結は「何も起きない」形で現れるので、
// 何がどう据え置かれたのかを毎回言う。
func freezeWarning(fz *channelFreeze) output.Warning {
	msg := fz.Channel + ": "
	switch fz.Mode {
	case freezeManifest:
		msg += "frozen at " + fz.Version + " — the manifest is rebuilt at that version, only the notice changes"
	case freezeScript:
		msg += "frozen at " + fz.Version + " — " + config.InstallScriptName + " keeps installing that version"
	default:
		if fz.Version != "" {
			msg += "frozen at " + fz.Version + " — "
		}
		msg += fz.Reason
	}
	return output.Warning{Code: output.WarnDeprecateFrozen, Message: msg}
}

// freezeKeepsArtifacts は そのチャネルの発行記録に成果物を残すか。凍結版でマニフェストを
// 作り直すチャネルだけが要る(手元の archive はビルドし直すたび版が上がる)。
func freezeKeepsArtifacts(ch string) bool { return freezeModes[ch] == freezeManifest }

// installScriptTarget は install.sh / install.ps1 が入れる版と、そもそも同梱するかを返す。
// script が ship:false なら最後に配った版のまま(告知だけが新しくなる)。配った版が無ければ
// 同梱しない — 凍結先が無いのに新版を配れば、止めたはずの経路から新版が漏れる。
func installScriptTarget(root string, cfg config.Config, version string) (v string, ship bool) {
	fz := loadFreeze(root, cfg, "script")
	switch {
	case fz == nil:
		return version, true
	case fz.Mode == freezeScript:
		return fz.Version, true
	default:
		return "", false
	}
}

// frozenArtifacts は凍結版の成果物を返す。凍結していなければ build が実成果物を作る(release を含む)。
// 凍結中に build を呼ばないことが肝心で、それが「新版を配らない」の実体になる。
func frozenArtifacts(fz *channelFreeze, build func() ([]build.Artifact, error)) ([]build.Artifact, error) {
	if fz != nil && fz.Mode == freezeManifest {
		return fz.Artifacts, nil
	}
	return build()
}

// writeInstallScripts は install.sh / install.ps1 を version 固定で .wharfy 配下に書く。
func writeInstallScripts(root string, cfg config.Config, version string) error {
	if _, err := config.WriteInstallScript(root, config.GenerateInstallScript(cfg, version)); err != nil {
		return err
	}
	_, err := config.WriteInstallPS1(root, config.GenerateInstallPS1(cfg, version))
	return err
}
