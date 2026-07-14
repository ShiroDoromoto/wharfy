package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ShiroDoromoto/wharfy/internal/attest"
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
	// Attest は付けた来歴(CI で証明できたときだけ出る)。手元では出ない=証明していない、と読める。
	Attest *attest.Result `json:"attest,omitempty"`
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
	in, loadErr := config.Load(root)
	if loadErr != nil {
		return configInvalidResult(c, loadErr)
	}
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
		return withStaleGeneratorWarning(root, c, res)
	}

	// apply: 版ズレ/tag/token が要る(release は実アップロード)。版ズレの検査はアップロードの前でなければ
	// 意味がない — 生成物は実行中のバイナリが作るので、上がってから告げても手遅れ。
	if res, blocked := staleGeneratorRefusal(root, c); blocked {
		return res
	}
	if tagMissing {
		return tagMissingResult(c, version)
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return tokenMissingResult(c)
	}
	// BYO(依頼①): GoReleaser を使わず、持ち込み成果物を自前で archive 化し GitHub Release へ上げる
	// (D-1: prebuilt builder は Pro 専用のため)。prebuilt(CLI)と bundle(GUI)は併用でき、両方
	// 宣言されていれば両方をリリースする(片方を黙って落とさない=依頼書四通目 依頼②)。
	var (
		archs []build.Artifact
		berr  error
	)
	if cfg.Prebuilt || cfg.Bundle {
		archs, berr = byoRelease(ctx, root, cfg, in, version)
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
	// 検知②: latest.json を同じ Release へ発行する(playbook §5)。Go/BYO 両経路が合流する
	// ここで1度だけ行えば、経路に依らず「新版あり」の横串が揃う。
	if err := uploadLatestJSON(ctx, root, cfg, version, archs); err != nil {
		return internalError(c, err)
	}
	// attest 段: 上げ切った成果物(実 sha256 が確定した後)に来歴を付ける。
	att, attWarn, attErr := attestRelease(ctx, cfg, archs)
	if attErr != nil {
		return buildErrorResult(c, attErr)
	}
	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		now := nowUTC().Format(time.RFC3339)
		st.Publish["releases"] = state.PublishRecord{Version: version, Target: cfg.Github, At: now}
		// script チャネルの実体(install.sh)はこの release が同梱アップロードしている。
		// その事実を台帳へ書かないと publish script を後追いするまで script 記録が古いまま残り、
		// status が実体(0.17.0)と記録(0.16.1)の drift=ahead を出す。releases と対で記録する。
		if config.HasChannel(cfg, "script") {
			st.Publish["script"] = state.PublishRecord{Version: version, Target: cfg.Github + " release:" + config.InstallScriptName, At: now}
		}
		_ = state.Save(root, st)
	}

	res := output.New(c.Name, releaseMessage(cfg, version, len(archs), att), true)
	res.Data = releaseData{Applied: true, Target: cfg.Github, Artifacts: archs, Attest: att}
	if attWarn != nil {
		res.Warnings = append(res.Warnings, *attWarn)
		res.Next = append(res.Next, output.NextDo{
			Reason: "attach build provenance to what you ship",
			Do:     "add permissions: id-token: write and attestations: write to the release workflow",
		})
	}
	res.Next = append(res.Next, nextFromSpec(c)...) // publish
	return withInitNudge(withStaleGeneratorWarning(root, c, res))
}

// releaseMessage は release の一行報告。来歴を付けたなら、付けたと言う——「証明したつもり」を
// 残さないために、証明の有無は成功メッセージの側に出す。
func releaseMessage(cfg config.Config, version string, n int, att *attest.Result) string {
	msg := "released " + cfg.Project + " " + version + ": " + strconv.Itoa(n) + " artifact(s) → " + cfg.Github
	if att != nil {
		msg += " (build provenance attested for " + strconv.Itoa(len(att.Subjects)) + " artifact(s))"
	}
	return msg
}

// byoRelease は BYO モード(prebuilt=CLI / bundle=GUI)の実リリース。両方宣言されていれば
// **両方**を同じ Release タグへ上げる(依頼書四通目=依頼②)。従来は if/else-if で bundle が
// prebuilt を握り潰し、CLI アーカイブが黙って欠落 → publish が 404 を指す Formula を書く事故に
// つながっていた。どちらか一方の宣言なら従来どおりその一方だけを出す。
// 署名失敗(prebuilt 経路)はそのまま error として返し、release を fail させる(未署名の半端リリースを作らない)。
func byoRelease(ctx context.Context, root string, cfg config.Config, in config.File, version string) ([]build.Artifact, error) {
	var archs []build.Artifact
	if cfg.Prebuilt {
		a, err := prebuiltRelease(ctx, root, cfg, in, version)
		if err != nil {
			return nil, err
		}
		archs = append(archs, a...)
	}
	if cfg.Bundle {
		// BYO-bundle(GUI・依頼①③): 持ち込みバンドル(.dmg/.exe/.AppImage/.deb/.rpm)を再 archive せず
		// そのまま Release アセットにする。AppImage はここで直 DL 可能になる(ポータブル・依頼③)。
		a, err := bundleRelease(ctx, root, cfg, version, in)
		if err != nil {
			return nil, err
		}
		archs = append(archs, a...)
	}
	return archs, nil
}

// uploadLatestJSON は検知②の latest.json を生成し、同じ Release タグへ資産として上げる
// (playbook §5)。全 release 経路(Go/BYO)共通の合流点で 1 度だけ行うため runRelease に置く。
// tag/GITHUB_TOKEN は apply 経路で保証済み。github(owner/repo)を組めない等の個別失敗では
// release 本体(バイナリは上がり済み)を壊さず skip する — latest.json は検知の付帯物ゆえ。
func uploadLatestJSON(ctx context.Context, root string, cfg config.Config, version string, archs []build.Artifact) error {
	owner, repo, ok := splitOwnerName(cfg.Github)
	if !ok {
		return nil // URL を組めない — 検知ファイルは skip(release 本体は成功済み)
	}
	assets := make([]config.LatestAsset, 0, len(archs))
	for _, a := range archs {
		assets = append(assets, config.LatestAsset{OS: a.OS, Arch: a.Arch, Name: filepath.Base(a.Path)})
	}
	content, ok := config.GenerateLatestJSON(cfg, version, assets)
	if !ok {
		return nil
	}
	p, err := config.WriteLatestJSON(root, content)
	if err != nil {
		return err
	}
	store := newReleaseStore(owner, repo, os.Getenv("GITHUB_TOKEN"))
	return store.Upload(ctx, "v"+version, cfg.Project+" "+version, []channel.ReleaseAsset{{
		Name:        config.LatestJSONName,
		Path:        p,
		ContentType: channel.AssetContentType(config.LatestJSONName),
	}})
}

// prebuiltRelease は BYO-binary の実リリース(依頼①)。持ち込みバイナリを archive 化し、
// script チャネルがあれば install.sh も生成し、GitHub Release へ(同名は置換して)アップロードする。
// 返す Archive 成果物(実 sha256)は Go 経路と同形なので、publish <ch> はこれをそのまま消費できる。
func prebuiltRelease(ctx context.Context, root string, cfg config.Config, in config.File, version string) ([]build.Artifact, error) {
	owner, repo, ok := splitOwnerName(cfg.Github)
	if !ok {
		return nil, fmt.Errorf("cannot resolve github owner/repo from %q", cfg.Github)
	}
	// sign 段(依頼①): identity があれば darwin バイナリを archive の**前**に署名する。
	// これで archive の checksum は署名後の実体を反映する(署名でハッシュが変わるため順序が要点)。
	bins, _, err := signPrebuiltBinaries(ctx, root, resolveSignOptions(in), toPrebuiltBinaries(in))
	if err != nil {
		return nil, err
	}
	archs, err := build.ArchivePrebuilt(root, config.DistDir, cfg.Project, version, prebuiltBinaryName(cfg, in), bins)
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
	// script チャネル: install.sh(mac/Linux)と install.ps1(Windows)を生成し release に同梱する
	// (Go 経路の extra_files 相当)。凍結(ship:false)なら入れる版は最後に配った版のまま。
	scriptVer, shipScript := installScriptTarget(root, cfg, version)
	if config.HasChannel(cfg, "script") && shipScript {
		p, err := config.WriteInstallScript(root, config.GenerateInstallScript(cfg, scriptVer))
		if err != nil {
			return nil, err
		}
		assets = append(assets, channel.ReleaseAsset{
			Name:        config.InstallScriptName,
			Path:        p,
			ContentType: channel.AssetContentType(config.InstallScriptName),
		})
		pp, err := config.WriteInstallPS1(root, config.GenerateInstallPS1(cfg, scriptVer))
		if err != nil {
			return nil, err
		}
		assets = append(assets, channel.ReleaseAsset{
			Name:        config.InstallPS1Name,
			Path:        pp,
			ContentType: channel.AssetContentType(config.InstallPS1Name),
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
