package main

import (
	"context"
	"os"

	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// runConfig は解決後の実効設定を返す(config.json)。
// 生ファイルではなく、既定推測を埋めた Config を data に載せる。
// 曖昧な main は output.ErrMainAmbiguous で停止する(ok=false・黙って間違えない)。
func runConfig(_ context.Context, c registry.Command, _ []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}

	in, loadErr := config.Load(root)

	// wharfy.yaml が不正でも、ファイルを無視して推測で best-effort 解決し data を満たす
	// (config.json は data 必須)。利用者には「無視して推測した実効設定」＋ config_invalid を見せる。
	effective := in
	if loadErr != nil {
		effective = config.File{}
	}
	cfg, rerr := config.NewResolver(root).Resolve(effective)

	if loadErr != nil {
		res := output.New(c.Name, "wharfy.yaml is invalid (showing inferred config)", false)
		res.Data = cfg
		res.Errors = []output.Problem{{
			Code:    output.ErrConfigInvalid,
			Message: loadErr.Error(),
			Hint:    configInvalidHint,
		}}
		res.Next = []output.NextDo{{Reason: "fix the file then re-run", Do: "wharfy config"}}
		return res
	}

	// main を解決できない(複数 main で曖昧、または検出コマンド `go list` が失敗)。
	// 実効設定は cfg に載っているので internal で潰さず、data + coded problem +
	// 次の一手(main を明示)を見せる。status が同じ Resolve のエラーを飲み込んで動くのに対し、
	// config は黙らず「main だけ未解決」と正直に見せる(黙って間違えない)。
	// go list 失敗の真因は rerr に stderr つきで包まれている(resolve.go execError)ので、
	// 利用者が見ていた不透明な "exit status 1" ではなく実メッセージが Message に出る。
	if rerr != nil {
		res := output.New(c.Name, "cannot resolve 'main'", false)
		res.Data = cfg // 部分解決した実効設定(config.json は data 必須・main は任意)
		res.Errors = []output.Problem{{
			Code:    output.ErrMainAmbiguous,
			Message: rerr.Error(),
			Hint:    "set 'main' in wharfy.yaml to the build target package (e.g. ./cmd/" + cfg.Project + ")",
		}}
		res.Next = []output.NextDo{{
			Reason: "set the build target so build can proceed",
			Do:     "echo 'main: ./cmd/" + cfg.Project + "' >> wharfy.yaml ; wharfy config",
		}}
		return res
	}

	res := output.New(c.Name, "resolved config for "+cfg.Project, true)
	res.Data = cfg
	res.Next = []output.NextDo{{Reason: "build with this config", Do: "wharfy build"}}
	return res
}

// configInvalidResult は読めない wharfy.yaml で停止する envelope(config 以外の全コマンド共通)。
// config だけは best-effort で推測した実効設定を見せる(上の runConfig)。他は進まない ——
// 設定が読めていないことに気づかないまま、既定で build / release / publish が走ってはいけない。
func configInvalidResult(c registry.Command, err error) output.Result {
	res := output.New(c.Name, "wharfy.yaml is invalid", false)
	res.Errors = []output.Problem{{
		Code:    output.ErrConfigInvalid,
		Message: err.Error(),
		Hint:    configInvalidHint,
	}}
	res.Next = []output.NextDo{{Reason: "see the resolved config once the file parses", Do: "wharfy config"}}
	return res
}

// configInvalidHint は wharfy.yaml が読めないときの次の一手(既知キーの一覧はスキーマが持つ)。
const configInvalidHint = "fix wharfy.yaml; see schemas/wharfy.config.json for known keys"

// internalError は想定外を envelope に包む(internal)。
func internalError(c registry.Command, err error) output.Result {
	res := output.New(c.Name, "internal error", false)
	res.Errors = []output.Problem{{Code: output.ErrInternal, Message: err.Error()}}
	res.Next = []output.NextDo{{Reason: "report this", Do: "open an issue with the message above"}}
	return res
}
