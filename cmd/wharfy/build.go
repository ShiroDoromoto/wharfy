package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// newBuilder は Builder の生成点(テストで差し替える＝末端は差し替え可能・01)。
var newBuilder = func(distDir string) build.Builder {
	return build.NewGoReleaserBuilder(distDir)
}

// nowUTC は時刻取得の差し替え点(記録の at をテストで固定する)。
var nowUTC = func() time.Time { return time.Now().UTC() }

// buildData は build の固有ペイロード(result.json の汎用 data に乗る)。
type buildData struct {
	Artifacts []build.Artifact `json:"artifacts"`
}

// runBuild は実効設定→生成 GoReleaser 設定→サブプロセスビルド→状態記録(設計 01/04/ADR-5)。
// 目印: `wharfy build --json` が成果物一覧と next: sign|publish を返す。
func runBuild(ctx context.Context, c registry.Command, _ []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}

	in, _ := config.Load(root) // 不正でも推測で進めず、解決の main 確定を優先(下で扱う)
	cfg, rerr := config.NewResolver(root).Resolve(in)
	var ambiguous *config.AmbiguousMainError
	if errors.As(rerr, &ambiguous) {
		res := output.New(c.Name, "cannot build: 'main' is ambiguous", false)
		res.Errors = []output.Problem{{
			Code:    output.ErrMainAmbiguous,
			Message: ambiguous.Error(),
			Hint:    "set 'main' in wharfy.yaml (e.g. ./cmd/" + cfg.Project + ")",
		}}
		res.Next = []output.NextDo{{Reason: "resolve the build target", Do: "wharfy config"}}
		return res
	}
	if rerr != nil {
		return internalError(c, rerr)
	}

	// 生成 GoReleaser 設定を .wharfy/ に書く(利用者 root には書かない・03)。
	glYAML, err := config.GenerateGoReleaser(cfg, in)
	if err != nil {
		return internalError(c, err)
	}
	configPath, err := config.WriteGoReleaser(root, glYAML)
	if err != nil {
		return internalError(c, err)
	}

	artifacts, berr := newBuilder(config.DistDir).Build(ctx, root, configPath)
	if berr != nil {
		return buildErrorResult(c, berr)
	}

	// ローカル記録に反映(速い基点。真実は status で実体照合・04)。
	if st, err := state.Load(root, cfg.Project); err == nil {
		st.RecordBuild(gitCurrentTag(root), nowUTC().Format(time.RFC3339), artifacts)
		_ = state.Save(root, st) // 記録失敗はビルド成功を覆さない(記録は最適化)
	}

	res := output.New(c.Name, buildMessage(artifacts), true)
	res.Data = buildData{Artifacts: artifacts}
	res.Next = nextFromSpec(c) // sign | publish
	if config.GitignoreNeedsWharfy(root) {
		res.Next = append(res.Next, output.NextDo{
			Reason: ".wharfy/ holds generated config and state; keep it out of git",
			Do:     "echo '.wharfy/' >> .gitignore",
		})
	}
	return res
}

// buildErrorResult は Builder のエラーを envelope のコードに変換する(09)。
func buildErrorResult(c registry.Command, berr error) output.Result {
	var unavailable *build.UnavailableError
	if errors.As(berr, &unavailable) {
		res := output.New(c.Name, "build tool unavailable", false)
		res.Errors = []output.Problem{{
			Code:    output.ErrBuilderUnavailable,
			Message: unavailable.Error(),
			Hint:    "install goreleaser: https://goreleaser.com/install/",
		}}
		res.Next = []output.NextDo{{Reason: "install the builder then retry", Do: "wharfy build"}}
		return res
	}
	var failed *build.FailedError
	if errors.As(berr, &failed) {
		res := output.New(c.Name, "build failed", false)
		hint := "see the goreleaser output above"
		if failed.Output != "" {
			hint = failed.Output
		}
		res.Errors = []output.Problem{{Code: output.ErrBuildFailed, Message: failed.Error(), Hint: hint}}
		res.Next = []output.NextDo{{Reason: "fix the error then retry", Do: "wharfy build"}}
		return res
	}
	return internalError(c, berr)
}

func buildMessage(artifacts []build.Artifact) string {
	n := len(artifacts)
	if n == 1 {
		return "built 1 artifact → " + config.DistDir
	}
	return "built " + strconv.Itoa(n) + " artifacts → " + config.DistDir
}

// gitCurrentTag は HEAD が指す厳密な tag を返す(無ければ空＝snapshot)。
func gitCurrentTag(root string) string {
	out, err := exec.Command("git", "-C", root, "describe", "--tags", "--exact-match").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
