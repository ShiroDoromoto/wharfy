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
	// Prerelease は、このリリースが prerelease である(資産は落とせるが latest ではない)。
	// 「上げた」と「利用者に届いた」を、出力の上で別の事実として読めるようにする。
	Prerelease bool `json:"prerelease,omitempty"`
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
		plan := "plan: upload the github release → " + target
		if flagPrerelease {
			plan += " (prerelease: not latest — users keep getting the old version)"
		}
		res := output.New(c.Name, plan, true)
		res.Data = releaseData{Applied: false, Target: cfg.Github, Prerelease: flagPrerelease}
		var next []output.NextDo
		for _, r := range reqs {
			if !r.Met {
				next = append(next, output.NextDo{Reason: "required before --yes: " + r.Requirement, Do: r.Hint})
			}
		}
		do := "wharfy release --yes"
		if flagPrerelease {
			do += " --prerelease"
		}
		res.Next = append(next, output.NextDo{Reason: "upload the release", Do: do})
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
	// 上げる前に、上げ先が何であるかを見る —— 窓は後からでは開けられない。既に latest として公開済みの
	// リリースへ --prerelease で上げ直せば、利用者が今まさに落としている資産を差し替えることになる。
	prior, perr := priorReleaseState(ctx, cfg, version)
	if perr != nil {
		return internalError(c, perr)
	}
	if flagPrerelease && prior.Exists && !prior.Prerelease {
		return releaseAlreadyPublicResult(c, cfg, version)
	}
	// このリリースが結局 prerelease になるか。--prerelease で作る場合と、既に prerelease として在る
	// タグへ上げ直す場合(公開状態は Upload では変えない)の両方。
	prerelease := flagPrerelease || prior.Prerelease
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
	manifests, err := uploadReleaseManifests(ctx, root, cfg, version, archs)
	if err != nil {
		return internalError(c, err)
	}
	// attest 段: 上げ切った物(実 sha256 が確定した後)に来歴を付ける。成果物だけでなく、release が
	// 配る install.sh / install.ps1 / latest.json も subject に入れる —— 利用者が実際に踏むのはそこ。
	att, attWarn, attErr := attestRelease(ctx, cfg, archs, releaseExtras(root, cfg, version, manifests))
	if attErr != nil {
		return buildErrorResult(c, attErr)
	}
	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		now := nowUTC().Format(time.RFC3339)
		st.Publish["releases"] = state.PublishRecord{Version: version, Target: cfg.Github, At: now, Prerelease: prerelease}
		// script チャネルの実体(install.sh)はこの release が同梱アップロードしている。
		// その事実を台帳へ書かないと publish script を後追いするまで script 記録が古いまま残り、
		// status が実体(0.17.0)と記録(0.16.1)の drift=ahead を出す。releases と対で記録する。
		// prerelease なら install.sh は上がっていても releases/latest/download/ からは引けない
		// ——「上げた」であって「配った」ではないので、記録にもその印を付ける。
		if config.HasChannel(cfg, "script") {
			st.Publish["script"] = state.PublishRecord{Version: version, Target: cfg.Github + " release:" + config.InstallScriptName, At: now, Prerelease: prerelease}
		}
		_ = state.Save(root, st)
	}

	res := output.New(c.Name, releaseMessage(cfg, version, len(archs), att, prerelease), true)
	res.Data = releaseData{Applied: true, Target: cfg.Github, Artifacts: archs, Prerelease: prerelease, Attest: att}
	if attWarn != nil {
		res.Warnings = append(res.Warnings, *attWarn)
		res.Next = append(res.Next, output.NextDo{
			Reason: "attach build provenance to what you ship",
			Do:     "add permissions: id-token: write and attestations: write to the release workflow",
		})
	}
	if prerelease {
		// 上げただけで配ってはいない、という事実を毎回言う。黙っていれば「リリースした」と読まれ、
		// 検証の窓は開いたまま忘れられる(そして利用者は旧版のままになる)。
		res.Warnings = append(res.Warnings, output.Warning{
			Code: output.WarnPrereleaseNotLatest,
			Message: "the release for v" + version + " is a prerelease: its assets download from their public URLs, " +
				"but it is not github's latest — releases/latest/download/ and latest.json still serve the previous version, so users are untouched",
		})
		res.Next = append(res.Next, output.NextDo{
			Reason: "verify the artifacts you will actually ship, while users still get the old version",
			Do:     "gh release download v" + version + " && wharfy verify --version " + version,
		}, output.NextDo{
			Reason: "hand it to users once it is green (this re-uploads nothing: the verified bytes are the shipped bytes)",
			Do:     "wharfy promote --yes",
		})
		return withInitNudge(withStaleGeneratorWarning(root, c, res))
	}
	res.Next = append(res.Next, nextFromSpec(c)...) // publish
	return withInitNudge(withStaleGeneratorWarning(root, c, res))
}

// priorReleaseState は、これから上げる tag のリリースが**既に在るか・prerelease か**を見る。
// github(owner/repo)を組めなければ判断材料が無いので「無い」として扱う(release 本体は続行する)。
func priorReleaseState(ctx context.Context, cfg config.Config, version string) (channel.ReleaseState, error) {
	owner, repo, ok := splitOwnerName(cfg.Github)
	if !ok {
		return channel.ReleaseState{}, nil
	}
	return newReleaseStore(owner, repo, os.Getenv("GITHUB_TOKEN")).Get(ctx, "v"+version)
}

// releaseOptions は今回のリリースに与える属性。**新しく作るときだけ**効く —— 既存のリリースの
// 公開状態を release が黙って切り替えることはない(切り替えは明示の工程が行う)。
func releaseOptions() channel.ReleaseOptions {
	return channel.ReleaseOptions{Prerelease: flagPrerelease}
}

// releaseAlreadyPublicResult は「latest として公開済みのリリースへ --prerelease で上げ直そうとした」拒否。
// 資産を置き換えれば、利用者が今まさに落としている物が変わる —— 検証の窓を開けるつもりの操作が
// 本番に触ってしまうので、上げる前に止める。
func releaseAlreadyPublicResult(c registry.Command, cfg config.Config, version string) output.Result {
	res := output.New(c.Name, "refusing to re-upload v"+version+" as a prerelease: it is already public", false)
	res.Errors = []output.Problem{{
		Code: output.ErrReleaseAlreadyPublic,
		Message: "the release for v" + version + " on " + cfg.Github + " is already published as the latest release: " +
			"uploading to it would replace the assets users are downloading right now",
		Hint: "cut the next version and release that one with --prerelease (a release cannot be un-published)",
	}}
	res.Next = []output.NextDo{{
		Reason: "open the verification window on a version users have not seen",
		Do:     "git tag vX.Y.Z && git push --tags   # then: wharfy release --yes --prerelease",
	}}
	return res
}

// releaseMessage は release の一行報告。来歴を付けたなら、付けたと言う——「証明したつもり」を
// 残さないために、証明の有無は成功メッセージの側に出す。prerelease も同じ理由で一行目に出す
// (「配った」と読ませない)。
func releaseMessage(cfg config.Config, version string, n int, att *attest.Result, prerelease bool) string {
	verb := "released "
	if prerelease {
		verb = "prereleased "
	}
	msg := verb + cfg.Project + " " + version + ": " + strconv.Itoa(n) + " artifact(s) → " + cfg.Github
	if prerelease {
		msg += " (not latest: users still get the previous version)"
	}
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

// uploadReleaseManifests は release が配る 2 つのマニフェストを、成果物と同じ Release へ上げる。
//
//   - latest.json — 「新版が出た」を利用者のプロダクトが知る口(検知②・playbook §5)
//   - artifacts.json — その Release が何を持っているか(資産名と実 sha256)。publish が別のジョブや
//     別のマシンで走ると手元の .wharfy/artifacts.json は無い。それを「記録が無い」と読んで release を
//     やり直せば、**検証したのとは別のバイト列**に貼り替わり、来歴もそのバイト列から外れる
//     ——「検証したものがそのまま配られる」を守るには、記録が Release 自身に乗っている必要がある。
//
// 全 release 経路(Go/BYO)の合流点で 1 度だけ行う。github(owner/repo)を組めない等の個別失敗では
// release 本体(成果物は上がり済み)を壊さず skip する — どちらも付帯物ゆえ。
// 返すのは実際に上げたローカルパス(skip した物は入らない)——来歴の subject にするため、上げた物と
// 上げなかった物をここで言い分ける。
func uploadReleaseManifests(ctx context.Context, root string, cfg config.Config, version string, archs []build.Artifact) ([]string, error) {
	owner, repo, ok := splitOwnerName(cfg.Github)
	if !ok {
		return nil, nil // URL を組めない — マニフェストは skip(release 本体は成功済み)
	}
	var (
		paths  []string
		assets []channel.ReleaseAsset
	)
	latestAssets := make([]config.LatestAsset, 0, len(archs))
	for _, a := range archs {
		latestAssets = append(latestAssets, config.LatestAsset{OS: a.OS, Arch: a.Arch, Name: filepath.Base(a.Path)})
	}
	if content, ok := config.GenerateLatestJSON(cfg, version, latestAssets); ok {
		p, err := config.WriteLatestJSON(root, content)
		if err != nil {
			return nil, err
		}
		paths = append(paths, p)
		assets = append(assets, channel.ReleaseAsset{
			Name:        config.LatestJSONName,
			Path:        p,
			ContentType: channel.AssetContentType(config.LatestJSONName),
		})
	}
	// artifacts.json は SaveArtifacts が既に書いている手元の記録そのもの(推測で組み直さない)。
	if p := filepath.Join(root, build.ArtifactsFile); fileExists(p) {
		paths = append(paths, p)
		assets = append(assets, channel.ReleaseAsset{
			Name:        channel.ManifestArtifacts,
			Path:        p,
			ContentType: channel.AssetContentType(channel.ManifestArtifacts),
		})
	}
	if len(assets) == 0 {
		return nil, nil
	}
	store := newReleaseStore(owner, repo, os.Getenv("GITHUB_TOKEN"))
	if err := store.Upload(ctx, "v"+version, cfg.Project+" "+version, assets, releaseOptions()); err != nil {
		return nil, err
	}
	return paths, nil
}

// fileExists は path に読める実体が在るか。
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
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
	if err := store.Upload(ctx, "v"+version, cfg.Project+" "+version, assets, releaseOptions()); err != nil {
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
	if err := store.Upload(ctx, "v"+version, cfg.Project+" "+version, assets, releaseOptions()); err != nil {
		return nil, &build.FailedError{Err: err}
	}
	return archs, nil
}
