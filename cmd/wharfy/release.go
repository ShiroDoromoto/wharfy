package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// releaseData は release の固有ペイロード(result.json の汎用 data に乗る)。
type releaseData struct {
	Applied   bool             `json:"applied"`
	Target    string           `json:"target,omitempty"`
	Artifacts []build.Artifact `json:"artifacts,omitempty"`
}

// runRelease は GitHub Release を作る独立工程(build→sign→release→publish の release)。
// アーカイブ/パッケージ/install.sh をアップロードし、成果物(実 sha256)を .wharfy/artifacts.json
// に記録する。publish <ch> はこれを消費してビルドし直さずにマニフェストを書ける(工程の分離)。
// container は GitHub Release でなくレジストリ push なので release の対象外(publish container)。
func runRelease(ctx context.Context, c registry.Command, _ []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}
	in, _ := config.Load(root)
	cfg, rerr := config.NewResolver(root).Resolve(in)
	var ambiguous *config.AmbiguousMainError
	if errors.As(rerr, &ambiguous) {
		return mainAmbiguousResult(c, cfg, ambiguous)
	}
	if rerr != nil {
		return internalError(c, rerr)
	}
	version, tagMissing := publishVersion(root)

	reqs := []requirement{
		{Requirement: "git tag", Met: !tagMissing, Hint: "git tag vX.Y.Z && git push --tags (the tag is the version)"},
		{Requirement: "GITHUB_TOKEN", Met: os.Getenv("GITHUB_TOKEN") != "", Hint: "export GITHUB_TOKEN=… (release upload)"},
	}

	if !flagYes {
		target := cfg.Github
		if target == "" {
			target = "(github unresolved)"
		}
		res := output.New(c.Name, "plan: upload the github release → "+target, true)
		res.Data = releaseData{Applied: false, Target: cfg.Github}
		var next []output.NextDo
		for _, r := range reqs {
			if !r.Met {
				next = append(next, output.NextDo{Reason: "required before --yes: " + r.Requirement, Do: r.Hint})
			}
		}
		res.Next = append(next, output.NextDo{Reason: "upload the release", Do: "wharfy release --yes"})
		return res
	}

	// apply: tag/token が要る(release は実アップロード)。
	if tagMissing {
		return tagMissingResult(c, version)
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return tokenMissingResult(c)
	}
	configPath, err := writeGeneratedConfig(root, cfg, in, version)
	if err != nil {
		return internalError(c, err)
	}
	// skipDocker=true: container はレジストリ push で release の範囲外(publish container が扱う)。
	archs, berr := newMultiReleaser(config.DistDir).ReleaseAll(ctx, root, configPath, true)
	if berr != nil {
		return buildErrorResult(c, berr)
	}
	// 成果物(実 sha)を記録 → publish <ch> はこれを消費して再ビルドしない。
	if err := build.SaveArtifacts(root, version, archs); err != nil {
		return internalError(c, err)
	}
	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		st.Publish["releases"] = state.PublishRecord{Version: version, Target: cfg.Github, At: nowUTC().Format(time.RFC3339)}
		_ = state.Save(root, st)
	}

	res := output.New(c.Name, "released "+cfg.Project+" "+version+": "+strconv.Itoa(len(archs))+" artifact(s) → "+cfg.Github, true)
	res.Data = releaseData{Applied: true, Target: cfg.Github, Artifacts: archs}
	res.Next = nextFromSpec(c) // publish
	return res
}
