package main

import (
	"context"

	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// handler はコマンド 1 つの本体。引数を解いてドメイン層を呼び Result を返す(設計 01)。
// スライス1 で中身が無いコマンドは stubResult を返す。
type handler func(ctx context.Context, c registry.Command, args []string) output.Result

// handlers は名前→本体。ここに無い登録コマンドは stub にフォールバックする。
// agent は別形(agent.json)なので root.go 側で特別扱いし、ここには載せない。
var handlers = map[string]handler{
	"version": runVersion,
	"config":  runConfig,
	"auth":    runAuth,
	"init":    runInit,
	"build":   runBuild,
	"release": runRelease,
	"publish": runPublish,
	"sign":    runSign,
	"verify":  runVerify,
}

// dispatch は registry エントリに対応する本体を選んで実行する。
func dispatch(ctx context.Context, c registry.Command, args []string) output.Result {
	if h, ok := handlers[c.Name]; ok {
		return h(ctx, c, args)
	}
	return stubResult(c)
}

// runVersion は tag を単一ソースにした版を返す(目印: `wharfy --json version` が envelope)。
// best-effort で最新リリースと比較し、新版があれば next: に upgrade を案内する(C: 更新導線)。
func runVersion(ctx context.Context, c registry.Command, _ []string) output.Result {
	v, commit, date := resolveVersion()
	res := output.New(c.Name, versionLine(), true)
	info := versionInfo{Version: v, Commit: commit, Date: date}

	if latest, ok := latestReleaseTag(ctx); ok {
		info.Latest = latest
		info.UpdateAvailable = updateAvailable(v, latest)
	}
	if info.UpdateAvailable {
		res.Next = append([]output.NextDo{{
			Reason: "newer wharfy available: " + info.Latest + " (you have " + v + "); brew/scoop users upgrade via your installer",
			Do:     "go install github.com/ShiroDoromoto/wharfy/cmd/wharfy@latest",
		}}, nextFromSpec(c)...)
	} else {
		res.Next = nextFromSpec(c)
	}
	res.Data = info
	return res
}

// stubResult は未実装コマンドの正直な戻り。envelope は valid に保つ(ok=true なので errors 不要)。
// スライス1 の実装順(08 §3)で順に本体へ差し替えていく。
func stubResult(c registry.Command) output.Result {
	res := output.New(c.Name, c.Name+": not implemented yet (slice 1 in progress)", true)
	res.Next = nextFromSpec(c)
	return res
}
