package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
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
	// BYO-binary(依頼①): GoReleaser を使わず、持ち込みバイナリを自前で archive 化し
	// GitHub Release へアップロードする(D-1: prebuilt builder は Pro 専用のため)。
	var (
		archs []build.Artifact
		berr  error
	)
	if cfg.Bundle {
		// BYO-bundle(GUI・依頼①③): 持ち込みバンドル(.dmg/.exe/.AppImage/.deb/.rpm)を再 archive せず
		// そのまま Release アセットにする。AppImage はここで直 DL 可能になる(ポータブル・依頼③)。
		archs, berr = bundleRelease(ctx, root, cfg, version, in)
	} else if cfg.Prebuilt {
		archs, berr = prebuiltRelease(ctx, root, cfg, in, version)
	} else {
		configPath, err := writeGeneratedConfig(root, cfg, in, version)
		if err != nil {
			return internalError(c, err)
		}
		// skipDocker=true: container はレジストリ push で release の範囲外(publish container が扱う)。
		archs, berr = newMultiReleaser(config.DistDir).ReleaseAll(ctx, root, configPath, true)
	}
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
	return withInitNudge(res)
}

// prebuiltRelease は BYO-binary の実リリース(依頼①)。持ち込みバイナリを archive 化し、
// script チャネルがあれば install.sh も生成し、GitHub Release へ(同名は置換して)アップロードする。
// 返す Archive 成果物(実 sha256)は Go 経路と同形なので、publish <ch> はこれをそのまま消費できる。
func prebuiltRelease(ctx context.Context, root string, cfg config.Config, in config.File, version string) ([]build.Artifact, error) {
	owner, repo, ok := splitOwnerName(cfg.Github)
	if !ok {
		return nil, fmt.Errorf("cannot resolve github owner/repo from %q", cfg.Github)
	}
	archs, err := build.ArchivePrebuilt(root, config.DistDir, cfg.Project, version, prebuiltBinaryName(cfg, in), toPrebuiltBinaries(in))
	if err != nil {
		return nil, err
	}

	assets := make([]channel.ReleaseAsset, 0, len(archs)+1)
	for _, a := range archs {
		name := filepath.Base(a.Path)
		assets = append(assets, channel.ReleaseAsset{
			Name:        name,
			Path:        filepath.Join(root, a.Path),
			ContentType: channel.AssetContentType(name),
		})
	}
	// script チャネル: install.sh を生成し release に同梱する(Go 経路の extra_files 相当)。
	if config.HasChannel(cfg, "script") {
		p, err := config.WriteInstallScript(root, config.GenerateInstallScript(cfg, version))
		if err != nil {
			return nil, err
		}
		assets = append(assets, channel.ReleaseAsset{
			Name:        config.InstallScriptName,
			Path:        p,
			ContentType: channel.AssetContentType(config.InstallScriptName),
		})
	}

	store := newReleaseStore(owner, repo, os.Getenv("GITHUB_TOKEN"))
	if err := store.Upload(ctx, "v"+version, cfg.Project+" "+version, assets); err != nil {
		return nil, &build.FailedError{Err: err}
	}
	return archs, nil
}

// bundleRelease は BYO-bundle の実リリース(GUI・依頼①)。持ち込みバンドルを再 archive せず、
// 存在＋実 sha256 を検証してそのまま GitHub Release へ(同名は置換して)アップロードする。
// アセット名は持ち込みファイル名をそのまま使い(cask の url もこれを参照する)、命名規約の齟齬を防ぐ。
// 返す成果物(Kind＋実 sha256)は cask がそのまま消費する(prebuiltRelease の対)。
func bundleRelease(ctx context.Context, root string, cfg config.Config, version string, in config.File) ([]build.Artifact, error) {
	owner, repo, ok := splitOwnerName(cfg.Github)
	if !ok {
		return nil, fmt.Errorf("cannot resolve github owner/repo from %q", cfg.Github)
	}
	archs, err := build.ValidateBundles(root, toBundles(in))
	if err != nil {
		return nil, err
	}
	assets := make([]channel.ReleaseAsset, 0, len(archs))
	for _, a := range archs {
		name := filepath.Base(a.Path)
		p := a.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, a.Path)
		}
		assets = append(assets, channel.ReleaseAsset{
			Name:        name,
			Path:        p,
			ContentType: channel.AssetContentType(name),
		})
	}
	store := newReleaseStore(owner, repo, os.Getenv("GITHUB_TOKEN"))
	if err := store.Upload(ctx, "v"+version, cfg.Project+" "+version, assets); err != nil {
		return nil, &build.FailedError{Err: err}
	}
	return archs, nil
}
