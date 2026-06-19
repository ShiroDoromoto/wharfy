package main

import (
	"context"
	"errors"
	"os"

	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// runConfig は解決後の実効設定を返す(設計 02 config.json / 07)。
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
			Hint:    "fix wharfy.yaml; see schemas/wharfy.config.json for known keys",
		}}
		res.Next = []output.NextDo{{Reason: "fix the file then re-run", Do: "wharfy config"}}
		return res
	}

	var ambiguous *config.AmbiguousMainError
	if errors.As(rerr, &ambiguous) {
		res := output.New(c.Name, "cannot resolve 'main' (ambiguous)", false)
		res.Data = cfg // 部分解決した実効設定(config.json は data 必須・main は任意)
		res.Errors = []output.Problem{{
			Code:    output.ErrMainAmbiguous,
			Message: ambiguous.Error(),
			Hint:    "set 'main' in wharfy.yaml to the build target package (e.g. ./cmd/" + cfg.Project + ")",
		}}
		res.Next = []output.NextDo{{
			Reason: "set the build target so build can proceed",
			Do:     "echo 'main: ./cmd/" + cfg.Project + "' >> wharfy.yaml ; wharfy config",
		}}
		return res
	}
	if rerr != nil {
		return internalError(c, rerr)
	}

	res := output.New(c.Name, "resolved config for "+cfg.Project, true)
	res.Data = cfg
	res.Next = []output.NextDo{{Reason: "build with this config", Do: "wharfy build"}}
	return res
}

// internalError は想定外を envelope に包む(09 internal)。
func internalError(c registry.Command, err error) output.Result {
	res := output.New(c.Name, "internal error", false)
	res.Errors = []output.Problem{{Code: output.ErrInternal, Message: err.Error()}}
	res.Next = []output.NextDo{{Reason: "report this", Do: "open an issue with the message above"}}
	return res
}
