package main

import (
	"fmt"
	"strings"

	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
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
