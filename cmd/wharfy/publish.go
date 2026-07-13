package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ShiroDoromoto/wharfy/internal/build"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// 差し替え点(テストで fake 化＝末端は差し替え可能)。
var (
	newArchiver      = func(distDir string) build.Archiver { return build.NewGoReleaserBuilder(distDir) }
	newReleaser      = func(distDir string) build.Releaser { return build.NewGoReleaserBuilder(distDir) }
	newPackager      = func(distDir string) build.Packager { return build.NewGoReleaserBuilder(distDir) }
	newContainerizer = func(distDir string) build.Containerizer { return build.NewGoReleaserBuilder(distDir) }
	newTapStore      = func(owner, repo, token string) channel.TapStore {
		return channel.NewGitHubTapStore(owner, repo, token)
	}
	newWingetSubmitter = func(token string) channel.Submitter { return channel.NewGitHubWingetSubmitter(token) }
	newCoreSubmitter   = func(token string) channel.CoreSubmitter { return channel.NewGitHubCoreSubmitter(token) }
	newAurPusher       = func(sshKey string) channel.AurPusher { return channel.NewGitAurPusher(sshKey) }
	newMultiReleaser   = func(distDir string) build.MultiReleaser { return build.NewGoReleaserBuilder(distDir) }
	// newReleaseStore は BYO-binary のネイティブ Release アップローダ(依頼①・テストで差し替え)。
	newReleaseStore = func(owner, repo, token string) channel.ReleaseStore {
		return channel.NewGitHubReleaseStore(owner, repo, token)
	}
	// newPrebuiltContainerizer は BYO-binary の buildx コンテナ生成(依頼① #3・テストで差し替え)。
	newPrebuiltContainerizer = func() *build.PrebuiltContainerizer { return build.NewPrebuiltContainerizer() }
	// newRegistryLogin は push 前の docker login(テストで差し替え)。
	newRegistryLogin = func() *build.RegistryLogin { return build.NewRegistryLogin() }
	// uploadPackage は hosted repo へ deb/rpm を上げる(テストで差し替え)。
	uploadPackage = httpUploadPackage
	// dockerAvailable は docker CLI の有無(container の前提・テストで差し替え)。
	dockerAvailable = func() bool { _, err := exec.LookPath("docker"); return err == nil }
	// goinstallProxy / scriptProbeURL / aurRPCBase / ociProbeBase はテストで実体照合先を
	// httptest に差し替える(空＝既定)。apt/rpm は cfg の repo URL をそのまま probe する。
	goinstallProxy  = ""
	scriptProbeURL  = ""
	aurRPCBase      = ""
	ociProbeBase    = ""
	wingetProbeBase = ""
)

// publishData は publish の固有ペイロード(schemas/publish.json data)。
type publishData struct {
	Applied  bool               `json:"applied"`
	Plan     []channel.PlanItem `json:"plan"`
	Requires []requirement      `json:"requires,omitempty"`
}

// requirement は実 apply(--yes)の前提条件と充足状況(publish.json requirement)。
// preview で出し、credential 無しのエージェントが1往復で apply 可否を判断できるようにする。
type requirement struct {
	Requirement string `json:"requirement"`
	Met         bool   `json:"met"`
	Hint        string `json:"hint,omitempty"`
}

// runPublish は所有チャネルへ発行する。書く前に必ず差分(plan)を見せる。
// --yes 無し: plan のプレビュー(applied:false)。--yes: 実書き込み(applied:true)。
// 実装済み: homebrew / goinstall。未対応チャネルは plan で skip を返す(型は同一)。
func runPublish(ctx context.Context, c registry.Command, args []string) output.Result {
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

	// apply(--yes)は tap/bucket へ実際に書く。マニフェストも実行中のバイナリが生成するので、
	// 版ズレのまま書けば古い生成器の産物が配られる。書く前に止める(plan は警告どまり)。
	if flagYes {
		if res, blocked := staleGeneratorRefusal(root, c); blocked {
			return res
		}
	}

	// 引数なし = 全チャネル一括(release は 1 回・多重 release 衝突を避ける)。
	if len(args) == 0 {
		return withStaleGeneratorWarning(root, c, publishAll(ctx, c, root, cfg, in, version, tagMissing))
	}

	// 名指しのチャネルも channels: の集合に閉じる。畳んだチャネルの repo は archive されていることが
	// 多く、そこへの書き戻しは手書きの廃止告知を潰す。復活は配布者が設定へ書き戻したときにだけ起こる。
	if config.KnownChannel(args[0]) && !config.HasChannel(cfg, args[0]) {
		return channelNotConfiguredResult(c, cfg, in, args[0])
	}

	// 凍結(ship:false)は結果の作り方ではなく入力を変える。据え置く版に差し替えてから発行し、
	// 何が据え置かれたかを結果に必ず載せる(不在は黙っていると気づかれない)。
	fz := loadFreeze(root, cfg, args[0])
	if fz != nil {
		if fz.Mode == freezeHold {
			return frozenHoldResult(c, fz)
		}
		if fz.Mode == freezeManifest {
			version = fz.Version
		}
	}
	res := withStaleGeneratorWarning(root, c, publishChannel(ctx, c, root, cfg, in, version, tagMissing, args[0], fz))
	if fz != nil {
		res.Warnings = append(res.Warnings, freezeWarning(fz))
	}
	return res
}

// channelNotConfiguredResult は channels: に無いチャネルを名指しされたときの拒否。
// 「まだ publish していない」ではなく「配ると宣言していない」ので、発行せず理由を返す。
func channelNotConfiguredResult(c registry.Command, cfg config.Config, in config.File, ch string) output.Result {
	item := channel.PlanItem{
		Channel: ch, Kind: config.Kind(ch), Action: channel.ActionSkip,
		Reason: "not in channels: — wharfy publishes only what you declared",
	}
	res := publishResult(c, ch+" is not in wharfy.yaml channels: — nothing published", false, []channel.PlanItem{item})
	res.Errors = []output.Problem{channelNotConfiguredProblem(cfg, in, ch)}
	res.Next = []output.NextDo{{Reason: "publish the channels you declared", Do: "wharfy publish --dry-run"}}
	return res
}

// channelNotConfiguredProblem は「その名前は channels: に無い」理由を組む(publish / verify 共用)。
// 拒む語彙を二度書かないため——どちらのコマンドでも、宣言した集合だけが対象という規則は同じ(D-4)。
func channelNotConfiguredProblem(cfg config.Config, in config.File, ch string) output.Problem {
	hint := "add '" + ch + "' back to channels: in wharfy.yaml if you mean to distribute it again"
	if config.IsPrebuilt(in) && !config.PrebuiltCompatible(ch) {
		hint = ch + " needs the Go toolchain and is dropped in prebuilt (BYO-binary) mode"
	}
	return output.Problem{
		Code:    output.ErrChannelNotConfigured,
		Message: ch + " is not a configured channel (declared: " + strings.Join(channelNames(cfg), ", ") + ")",
		Hint:    hint,
	}
}

// channelNames は解決済みチャネルの名前列(拒否理由に「宣言した集合」を載せるため)。
func channelNames(cfg config.Config) []string {
	out := make([]string, 0, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		out = append(out, ch.Name)
	}
	return out
}

// frozenHoldResult は「配るのを止めた」チャネルの結果。書き込みも release も走らせない。
func frozenHoldResult(c registry.Command, fz *channelFreeze) output.Result {
	item := channel.PlanItem{Channel: fz.Channel, Kind: config.Kind(fz.Channel), Action: channel.ActionNoop, Reason: fz.Reason}
	res := publishResult(c, fz.Channel+" is frozen — nothing to publish", true, []channel.PlanItem{item})
	res.Warnings = []output.Warning{freezeWarning(fz)}
	res.Next = []output.NextDo{{Reason: "keep shipping this channel while announcing", Do: "set deprecate." + fz.Channel + ".ship: true"}}
	return res
}

// publishChannel は単体チャネルの発行にディスパッチする。fz は凍結の解決結果(非凍結なら nil)。
func publishChannel(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool, ch string, fz *channelFreeze) output.Result {
	// 凍結版の成果物を渡すのはマニフェストを作り直すチャネルだけ(script は release 自体は新版で走る)。
	var manifestFreeze *channelFreeze
	if fz != nil && fz.Mode == freezeManifest {
		manifestFreeze = fz
	}
	switch ch {
	case "homebrew":
		return publishHomebrew(ctx, c, root, cfg, in, version, tagMissing, manifestFreeze)
	case "cask":
		return publishCask(ctx, c, root, cfg, in, version, tagMissing, manifestFreeze)
	case "scoop":
		return publishScoop(ctx, c, root, cfg, in, version, tagMissing, manifestFreeze)
	case "apt":
		return publishLinuxPkg(ctx, c, root, cfg, in, version, tagMissing, "apt", ".deb")
	case "rpm":
		return publishLinuxPkg(ctx, c, root, cfg, in, version, tagMissing, "rpm", ".rpm")
	case "container":
		return publishContainer(ctx, c, root, cfg, in, version, tagMissing)
	case "winget":
		return publishWinget(ctx, c, root, cfg, in, version, tagMissing)
	case "homebrew-core":
		return publishHomebrewCore(ctx, c, root, cfg, in, version, tagMissing)
	case "aur":
		return publishAur(ctx, c, root, cfg, in, version, tagMissing, manifestFreeze)
	case "goinstall":
		return publishGoinstall(ctx, c, root, cfg, tagMissing)
	case "script":
		return publishScript(ctx, c, root, cfg, in, version, tagMissing)
	default:
		item := channel.PlanItem{
			Channel: ch, Action: channel.ActionSkip,
			Reason: "unknown channel (owned: homebrew/cask/scoop/apt/rpm/container/aur/script/goinstall, gated: winget/homebrew-core)",
		}
		res := publishResult(c, "channel "+ch+" not implemented", false, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "publish a supported channel or all", Do: "wharfy publish"}}
		return res
	}
}

// implementedChannels は cfg.Channels のうち publish が扱える順序付きリスト。
func implementedChannels(cfg config.Config) []string {
	known := map[string]bool{
		"homebrew": true, "cask": true, "scoop": true, "apt": true, "rpm": true, "container": true,
		"aur": true, "winget": true, "goinstall": true, "script": true, "releases": true,
		"homebrew-core": true,
	}
	var out []string
	for _, ch := range cfg.Channels {
		if known[ch.Name] {
			out = append(out, ch.Name)
		}
	}
	return out
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// unionRequires は一括 publish の前提条件を、構成チャネルから合算する。
func unionRequires(chans []string, tagMissing bool) []requirement {
	reqs := []requirement{
		{Requirement: "git tag", Met: !tagMissing, Hint: "git tag vX.Y.Z && git push --tags (the tag is the version)"},
	}
	// goinstall 以外は GitHub Releases(ReleaseAll)を要する。
	needsRelease := false
	for _, ch := range chans {
		if ch != "goinstall" {
			needsRelease = true
		}
	}
	if needsRelease {
		reqs = append(reqs, requirement{Requirement: "GITHUB_TOKEN", Met: os.Getenv("GITHUB_TOKEN") != "", Hint: "export GITHUB_TOKEN=… (release upload / fork+PR)"})
	}
	if containsStr(chans, "apt") || containsStr(chans, "rpm") {
		reqs = append(reqs, requirement{Requirement: "PACKAGE_REPO_TOKEN", Met: resolveToken("PACKAGE_REPO_TOKEN", "package_repo_token") != "", Hint: "export PACKAGE_REPO_TOKEN=… or run: wharfy auth fury (keychain)"})
	}
	if containsStr(chans, "aur") {
		reqs = append(reqs, requirement{Requirement: "AUR_SSH_KEY", Met: os.Getenv("AUR_SSH_KEY") != "", Hint: "export AUR_SSH_KEY=… (aur push)"})
	}
	if containsStr(chans, "container") {
		reqs = append(reqs, requirement{Requirement: "docker", Met: dockerAvailable(), Hint: "install Docker (with buildx)"})
	}
	return reqs
}

// publishAll は全チャネルを一括発行する。release は 1 回(ReleaseAll)だけ走らせ、各チャネルの
// 書き込み(formula/manifest/upload/PR)をその成果物に対して行う(多重 release 衝突を避ける)。
func publishAll(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool) output.Result {
	chans := implementedChannels(cfg)
	if len(chans) == 0 {
		item := channel.PlanItem{Channel: "(all)", Action: channel.ActionSkip, Reason: "no implemented channels in config"}
		res := publishResult(c, "nothing to publish", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "add channels", Do: "wharfy config"}}
		return res
	}
	reqs := unionRequires(chans, tagMissing)

	if !flagYes {
		// 一括 preview は軽量サマリ(各チャネルの発行先と操作)。詳細差分は単体 publish <ch> --dry-run。
		// 凍結は apply で初めて分かると驚くので、ここで先に見せる。
		prevSt, _ := state.Load(root, cfg.Project)
		var items []channel.PlanItem
		var frozenWarns []output.Warning
		for _, ch := range chans {
			it := planChannelSummary(ch, cfg, in)
			if fz := resolveFreeze(cfg, prevSt, ch); fz != nil {
				frozenWarns = append(frozenWarns, freezeWarning(fz))
				if fz.Mode == freezeHold {
					it.Action, it.Reason, it.OwnedArtifact = channel.ActionNoop, fz.Reason, ""
				}
			}
			items = append(items, it)
		}
		res := output.New(c.Name, fmt.Sprintf("plan: %d channel(s)", len(items)), true)
		res.Data = publishData{Applied: false, Plan: items, Requires: reqs}
		res.Warnings = frozenWarns
		next := []output.NextDo{}
		for _, r := range reqs {
			if !r.Met {
				next = append(next, output.NextDo{Reason: "required before --yes: " + r.Requirement, Do: r.Hint})
			}
		}
		res.Next = append(next, output.NextDo{Reason: "apply all channels (one release)", Do: "wharfy publish --yes"})
		return res
	}

	// apply: release を 1 回だけ走らせるための前提。
	if tagMissing {
		return tagMissingResult(c, version)
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return tokenMissingResult(c)
	}
	ghOwner, ghRepo, _ := splitOwnerName(cfg.Github)
	dockerOK := dockerAvailable()
	// container が凍結なら image も push しない(release 側で焼くので、ここで降ろす)。
	skipDocker := !(containsStr(chans, "container") && dockerOK) || frozenChannel(cfg, "container")

	configPath, err := writeGeneratedConfig(root, cfg, in, version)
	if err != nil {
		return internalError(c, err)
	}
	// image を push するなら、その前にレジストリへログインする(単発の publish container と同じ)。
	// goreleaser の docker pipe も docker の資格情報に乗るので、ここが両経路の合流点になる。
	if !skipDocker {
		if lerr := loginToImageRegistry(ctx, cfg); lerr != nil {
			return buildErrorResult(c, lerr)
		}
	}
	// release が済んでいれば(同 version)再アップロードしない(c2)。途中失敗からの再開で高コストな
	// release を繰り返さない土台。無ければ 1 回だけ走らせて記録する。
	var archs []build.Artifact
	released := false
	if set, found, _ := build.LoadArtifacts(root); found && set.Version == version {
		archs = set.Artifacts
	} else if cfg.Prebuilt || cfg.Bundle {
		// BYO(依頼①②): GoReleaser を通さず、自前 archive＋ネイティブ Release upload。
		// prebuilt(CLI)と bundle(GUI)は併用でき、両方あれば両方を出す(片方を落とさない)。
		a, rerr := byoRelease(ctx, root, cfg, in, version)
		if rerr != nil {
			return buildErrorResult(c, rerr)
		}
		_ = build.SaveArtifacts(root, version, a)
		archs = a
		released = true
	} else {
		a, rerr := newMultiReleaser(config.DistDir).ReleaseAll(ctx, root, configPath, skipDocker)
		if rerr != nil {
			return buildErrorResult(c, rerr)
		}
		_ = build.SaveArtifacts(root, version, a)
		archs = a
		released = true
	}
	// release をここで内包したなら latest.json も同じ Release へ出す(runRelease と同じ合流点)。
	// 出さないと、release を独立に叩かず publish だけで配ったリリースから latest.json が落ちる。
	if released {
		if err := uploadLatestJSON(ctx, root, cfg, version, archs); err != nil {
			return internalError(c, err)
		}
	}

	st, _ := state.Load(root, cfg.Project)
	if st.Publish == nil {
		st.Publish = map[string]state.PublishRecord{}
	}
	now := nowUTC().Format(time.RFC3339)

	var items []channel.PlanItem
	var warns []output.Warning
	for _, ch := range chans {
		// 凍結(ship:false)なら配るのは新版ではなく最後に配った版。生成器へ渡す版と成果物を差し替える。
		chVersion, chArchs := version, archs
		fz := resolveFreeze(cfg, st, ch)
		if fz != nil {
			warns = append(warns, freezeWarning(fz))
			switch fz.Mode {
			case freezeHold:
				items = append(items, channel.PlanItem{Channel: ch, Kind: config.Kind(ch), Action: channel.ActionNoop, Reason: fz.Reason})
				continue
			case freezeManifest:
				chVersion, chArchs = fz.Version, fz.Artifacts
			case freezeScript:
				// release は新版で走った。install.sh が入れる版(writeGeneratedConfig で凍結済み)を記録する。
				chVersion = fz.Version
			}
		}
		// state 認識の再開(b): その version で発行済みのチャネルは飛ばす。途中失敗後の再実行で
		// 完了済みを再処理しない(残った失敗チャネルだけ進む)。凍結中のマニフェストは告知を載せ直す
		// ために毎回作り直す(内容が同じなら Publish 自身が noop になる)。
		if rec, ok := st.Publish[ch]; ok && rec.Version == version && (fz == nil || fz.Mode != freezeManifest) {
			items = append(items, channel.PlanItem{Channel: ch, Kind: config.Kind(ch), Action: channel.ActionNoop, Reason: "already published at " + version})
			continue
		}
		item, w, aerr := applyChannel(ctx, ch, root, cfg, in, chVersion, ghOwner, ghRepo, chArchs, st, now, dockerOK)
		if aerr != nil {
			// 1 チャネルの失敗は全体を止める。release と完了チャネルは記録済みなので、再実行は
			// 残りだけを安全・安価に進める(release は再アップロードしない)。
			res := output.New(c.Name, "publish failed at "+ch, false)
			res.Errors = []output.Problem{{Code: output.ErrPublishFailed, Message: ch + ": " + aerr.Error(), Hint: "fix and re-run; release/other channels already applied"}}
			res.Next = []output.NextDo{{Reason: "resume the batch (skips completed)", Do: "wharfy publish --yes"}}
			_ = state.Save(root, st)
			return res
		}
		items = append(items, item)
		if w != nil {
			warns = append(warns, *w)
		}
	}
	_ = state.Save(root, st)

	res := publishResult(c, fmt.Sprintf("published %d channel(s) at %s", len(items), version), true, items)
	res.Data = publishData{Applied: true, Plan: items}
	res.Warnings = append(warns, deprecationWarnings(cfg)...)
	res.Next = []output.NextDo{{Reason: "verify installs work", Do: "wharfy verify"}}
	return withInitNudge(res)
}

// planChannelSummary は一括 preview 用の軽量 plan(発行先＋操作。差分は出さない)。
func planChannelSummary(ch string, cfg config.Config, in config.File) channel.PlanItem {
	target := channelTargetByName(cfg, ch)
	it := channel.PlanItem{Channel: ch, Kind: config.Kind(ch), Action: channel.ActionCreate}
	switch ch {
	case "homebrew":
		it.OwnedArtifact = orUnresolved(target, "Formula/"+cfg.Project+".rb")
	case "cask":
		it.OwnedArtifact = orUnresolved(target, "Casks/"+caskToken(cfg, in)+".rb")
	case "scoop":
		it.OwnedArtifact = orUnresolved(target, "bucket/"+scoopToken(cfg, in)+".json")
	case "apt", "rpm":
		if target == "" {
			it.Action, it.Reason = channel.ActionSkip, ch+".repo not set"
		} else {
			it.OwnedArtifact = target
		}
	case "container":
		it.OwnedArtifact = orUnresolved(target, "(image)")
	case "aur":
		it.OwnedArtifact = "aur:" + orUnresolved(target, "(pkg)")
	case "winget":
		it.Action, it.OwnedArtifact = channel.ActionPrepare, "microsoft/winget-pkgs (PR)"
	case "homebrew-core":
		it.Action, it.OwnedArtifact = channel.ActionPrepare, "Homebrew/homebrew-core (PR)"
	case "script":
		it.OwnedArtifact = cfg.Github + " release:" + config.InstallScriptName
	case "releases":
		it.OwnedArtifact = orUnresolved(cfg.Github, "(releases)")
	case "goinstall":
		it.Action, it.Reason = channel.ActionNoop, "advisory (go install)"
	}
	return it
}

func orUnresolved(target, suffix string) string {
	if target == "" {
		return "(unresolved):" + suffix
	}
	return target + ":" + suffix
}

// applyChannel は 1 チャネルを共有 archs に対して書き込み、状態を更新する(release は呼ばない)。
func applyChannel(ctx context.Context, ch, root string, cfg config.Config, in config.File, version, ghOwner, ghRepo string, archs []build.Artifact, st *state.State, now string, dockerOK bool) (channel.PlanItem, *output.Warning, error) {
	mk := func(kind, action, art string) channel.PlanItem {
		return channel.PlanItem{Channel: ch, Kind: kind, Action: action, OwnedArtifact: art}
	}
	skip := func(reason string) (channel.PlanItem, *output.Warning, error) {
		return channel.PlanItem{Channel: ch, Kind: config.Kind(ch), Action: channel.ActionSkip, Reason: reason},
			&output.Warning{Code: output.WarnChannelSkipped, Message: ch + " skipped — " + reason}, nil
	}

	switch ch {
	case "releases":
		st.Publish["releases"] = state.PublishRecord{Version: version, Target: cfg.Github, At: now}
		return mk(channel.KindOwned, channel.ActionUpdate, cfg.Github), nil, nil

	case "homebrew":
		tap, ok := homebrewTarget(cfg)
		to, tr, ok2 := splitOwnerName(tap)
		if !ok || !ok2 {
			return skip("tap unresolved")
		}
		hb := homebrewPublisher(cfg, in, tap, to, tr, ghOwner, ghRepo, version, archs)
		if err := verifyManifestChecksums(hb, archs); err != nil { // #10: 書き込み前の自己検査(tap 作成より前で止める)
			return channel.PlanItem{}, nil, err
		}
		if _, err := hb.EnsureRepo(ctx); err != nil { // 未作成なら tap を作る
			return channel.PlanItem{}, nil, err
		}
		item, pub, err := hb.Publish(ctx)
		if err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish["homebrew"] = state.PublishRecord{Version: version, Target: tap, Commit: pub.Commit, At: now, Artifacts: archs}
		item.Action = channel.ActionUpdate
		return item, nil, nil

	case "cask":
		tap := channelTargetByName(cfg, "cask")
		to, tr, ok := splitOwnerName(tap)
		if !ok {
			return skip("cask tap unresolved")
		}
		ck := caskPublisher(cfg, in, tap, to, tr, ghOwner, ghRepo, version, archs)
		if err := verifyManifestChecksums(ck, archs); err != nil { // #10: 書き込み前の自己検査
			return channel.PlanItem{}, nil, err
		}
		if _, err := ck.EnsureRepo(ctx); err != nil { // 未作成なら tap を作る
			return channel.PlanItem{}, nil, err
		}
		item, pub, err := ck.Publish(ctx)
		if err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish["cask"] = state.PublishRecord{Version: version, Target: tap, Commit: pub.Commit, At: now, Artifacts: archs}
		item.Action = channel.ActionUpdate
		w := caskNotarizeWarning(cfg, in) // 依頼⑤: 非 notarized を一括発行でも先出し
		return item, &w, nil

	case "scoop":
		bucket := channelTargetByName(cfg, "scoop")
		bo, br, ok := splitOwnerName(bucket)
		if !ok {
			return skip("bucket unresolved")
		}
		sc := scoopPublisher(cfg, in, bucket, bo, br, ghOwner, ghRepo, version, archs)
		if err := verifyManifestChecksums(sc, archs); err != nil { // #10: 書き込み前の自己検査
			return channel.PlanItem{}, nil, err
		}
		if _, err := sc.EnsureRepo(ctx); err != nil { // 未作成なら bucket を作る
			return channel.PlanItem{}, nil, err
		}
		item, pub, err := sc.Publish(ctx)
		if err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish["scoop"] = state.PublishRecord{Version: version, Target: bucket, Commit: pub.Commit, At: now, Artifacts: archs}
		item.Action = channel.ActionUpdate
		return item, nil, nil

	case "apt", "rpm":
		repo := channelTargetByName(cfg, ch)        // 配信(probe/install/記録)
		pushURL := channelPushTargetByName(cfg, ch) // アップロード先(fury は別ホスト)
		if pushURL == "" {
			return skip(ch + " has no hosted repo configured")
		}
		if repo == "" {
			repo = pushURL
		}
		token := resolveToken("PACKAGE_REPO_TOKEN", "package_repo_token")
		if token == "" {
			return skip("PACKAGE_REPO_TOKEN not set")
		}
		ext := map[string]string{"apt": ".deb", "rpm": ".rpm"}[ch]
		if _, err := uploadLinuxPackages(ctx, archs, ext, pushURL, token); err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish[ch] = state.PublishRecord{Version: version, Target: repo, At: now}
		// 上げただけでは配れていないことがある(索引待ち/非公開のまま)。上げた直後にそれを言う。
		return mk(channel.KindOwned, channel.ActionUpdate, repo), pkgNotIndexedWarning(ctx, ch, repo, cfg.Project, version), nil

	case "container":
		image := channelTargetByName(cfg, "container")
		if !dockerOK {
			return skip("docker unavailable")
		}
		// GoReleaser 経路なら ReleaseAll の docker pipe が push 済み。BYO(prebuilt)は byoRelease が
		// archive と Release upload しかしないので、イメージはここで push する——さもないと push して
		// いないのに published と記録され、state が嘘をつく。
		switch {
		case cfg.Prebuilt:
			if cerr := newPrebuiltContainerizer().PushMultiArch(ctx, root, config.DistDir, image, version,
				prebuiltBinaryName(cfg, in), toPrebuiltBinaries(in)); cerr != nil {
				return channel.PlanItem{}, nil, cerr
			}
		case cfg.Bundle:
			// bundle だけ(GUI)は詰めるべき linux バイナリを持たない。記録もしない。
			return skip("container needs prebuilt linux binaries")
		}
		st.Publish["container"] = state.PublishRecord{Version: version, Target: image, At: now}
		return mk(channel.KindOwned, channel.ActionUpdate, image), nil, nil

	case "script":
		st.Publish["script"] = state.PublishRecord{Version: version, Target: cfg.Github + " release:" + config.InstallScriptName, At: now}
		return mk(channel.KindOwned, channel.ActionUpdate, cfg.Github+" release:"+config.InstallScriptName), nil, nil // ReleaseAll が install.sh を同梱済み

	case "aur":
		pkg := channelTargetByName(cfg, "aur")
		sshKey := os.Getenv("AUR_SSH_KEY")
		if sshKey == "" {
			return skip("AUR_SSH_KEY not set")
		}
		aurDeps, aurOpt := config.AurDeps(in)
		ai := channel.AurInput{Package: pkg, Project: cfg.Project, Version: version, License: cfg.License,
			Description: in.Description, Homepage: cfg.Homepage, Maintainer: aurMaintainer(ghOwner),
			Depends: aurDeps, OptDepends: aurOpt, Notice: channelNotice(cfg, "aur"),
			Sources: aurSources(archs, ghOwner, ghRepo, cfg.Project, version)}
		commit, err := newAurPusher(sshKey).Push(ctx, pkg, ai.Files())
		if err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish["aur"] = state.PublishRecord{Version: version, Target: pkg, Commit: commit, At: now, Artifacts: archs}
		return mk(channel.KindOwned, channel.ActionUpdate, "aur:"+pkg), nil, nil

	case "winget":
		identifier := channelTargetByName(cfg, "winget")
		wi := channel.WingetInput{Identifier: identifier, Project: cfg.Project, Version: version, License: cfg.License,
			Description: in.Description, Homepage: cfg.Homepage, Installers: wingetInstallersFor(cfg, archs, ghOwner, ghRepo, version)}
		prURL, err := newWingetSubmitter(os.Getenv("GITHUB_TOKEN")).Submit(ctx, wi, channel.GenerateWingetManifests(wi))
		if err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish["winget"] = state.PublishRecord{Version: version, Target: identifier, State: "pr_open", PR: prURL, At: now}
		return mk(channel.KindGated, channel.ActionPrepare, "microsoft/winget-pkgs (PR)"),
			&output.Warning{Code: output.WarnGatedPending, Message: "winget PR awaiting review: " + prURL}, nil

	case "homebrew-core":
		// strict gated: 明示同意が無ければ batch を止めず skip(誤申請でメンテナを煩わせない)。
		if !flagAckReview {
			return channel.PlanItem{Channel: "homebrew-core", Kind: channel.KindGated, Action: channel.ActionSkip,
					Reason: "needs --acknowledge-review (strict review)"},
				&output.Warning{Code: output.WarnChannelSkipped, Message: "homebrew-core skipped — needs --acknowledge-review (" + strictGated["homebrew-core"].criteria + ")"}, nil
		}
		central := channelTargetByName(cfg, "homebrew-core")
		sha, serr := sourceTarballSHA(ctx, sourceTarballURL(ghOwner, ghRepo, version))
		if serr != nil {
			return channel.PlanItem{}, nil, serr
		}
		formula := channel.GenerateCoreFormula(channel.CoreFormulaInput{
			Project: cfg.Project, Binary: cfg.Project, Main: cfg.Main, Description: in.Description, Homepage: cfg.Homepage,
			License: cfg.License, Version: version, Dependencies: homebrewDeps(in),
			SourceURL: sourceTarballURL(ghOwner, ghRepo, version), SourceSHA: sha})
		prURL, err := newCoreSubmitter(os.Getenv("GITHUB_TOKEN")).Submit(ctx, channel.CoreInput{
			Central: central, Project: cfg.Project, Version: version,
			FormulaFile: channel.CoreFormulaPath(cfg.Project), Formula: formula})
		if err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish["homebrew-core"] = state.PublishRecord{Version: version, Target: central, State: "pr_open", PR: prURL, At: now}
		return mk(channel.KindGated, channel.ActionPrepare, "Homebrew/homebrew-core (PR)"),
			&output.Warning{Code: output.WarnGatedPending, Message: "homebrew-core PR awaiting review: " + prURL}, nil

	case "goinstall":
		// 梱包ゼロ。release 不要・書き込みなし。記録もしない(advisory)。
		return mk(channel.KindOwned, channel.ActionNoop, ""), nil, nil
	}
	return channel.PlanItem{Channel: ch, Action: channel.ActionSkip, Reason: "unknown"}, nil, nil
}

// uploadLinuxPackages は archs の deb/rpm を hosted repo へ上げ、件数を返す。
func uploadLinuxPackages(ctx context.Context, archs []build.Artifact, ext, repo, token string) (int, error) {
	n := 0
	for _, a := range archs {
		if filepath.Ext(a.Path) != ext {
			continue
		}
		if err := uploadPackage(ctx, repo, token, a.Path); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// publishHomebrew / publishScoop は archive を要する owned チャネル。tap/bucket(自前リポジトリ)
// に formula/manifest を書く。型は共通(publishViaRelease)で、Publisher の組み立てだけ差し替える。
func publishHomebrew(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool, fz *channelFreeze) output.Result {
	tap, ok := homebrewTarget(cfg)
	tapOwner, tapRepo, tapOK := splitOwnerName(tap)
	ghOwner, ghRepo, ghOK := splitOwnerName(cfg.Github)
	if !ok || !tapOK || !ghOK {
		return ownedSkip(c, "homebrew", "homebrew tap/github unresolved — set 'github' or 'homebrew.tap' in wharfy.yaml")
	}
	return publishViaRelease(ctx, c, root, cfg, in, version, tagMissing, "homebrew", tap, fz,
		func(archs []build.Artifact) channel.Publisher {
			return homebrewPublisher(cfg, in, tap, tapOwner, tapRepo, ghOwner, ghRepo, version, archs)
		})
}

func publishScoop(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool, fz *channelFreeze) output.Result {
	bucket := channelTargetByName(cfg, "scoop")
	bOwner, bRepo, bOK := splitOwnerName(bucket)
	ghOwner, ghRepo, ghOK := splitOwnerName(cfg.Github)
	if bucket == "" || !bOK || !ghOK {
		return ownedSkip(c, "scoop", "scoop bucket/github unresolved — set 'github' or 'scoop.bucket' in wharfy.yaml")
	}
	return publishViaRelease(ctx, c, root, cfg, in, version, tagMissing, "scoop", bucket, fz,
		func(archs []build.Artifact) channel.Publisher {
			return scoopPublisher(cfg, in, bucket, bOwner, bRepo, ghOwner, ghRepo, version, archs)
		})
}

// publishAur は aur チャネル(owned)。-bin パッケージの PKGBUILD/.SRCINFO を生成し、
// AUR の自前 git(ssh)へ push する(審査なし)。linux tarball の実 sha を参照する。
func publishAur(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool, fz *channelFreeze) output.Result {
	pkg := channelTargetByName(cfg, "aur")
	ghOwner, ghRepo, ghOK := splitOwnerName(cfg.Github)
	if pkg == "" || !ghOK {
		item := channel.PlanItem{Channel: "aur", Kind: channel.KindOwned, Action: channel.ActionSkip,
			Reason: "aur package/github unresolved — set 'github' or 'aur.package'"}
		res := publishResult(c, "aur skipped — unresolved", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "check the resolved config", Do: "wharfy config"}}
		return res
	}

	// 凍結中はビルドも release も走らないので生成設定は要らない(publishViaRelease と同じ理由)。
	var configPath string
	if fz == nil {
		var err error
		configPath, err = writeGeneratedConfig(root, cfg, in, version)
		if err != nil {
			return internalError(c, err)
		}
	}
	aurDeps, aurOpt := config.AurDeps(in)
	buildInput := func(archs []build.Artifact) channel.AurInput {
		return channel.AurInput{
			Package:     pkg,
			Project:     cfg.Project,
			Version:     version,
			License:     cfg.License,
			Description: in.Description,
			Homepage:    cfg.Homepage,
			Maintainer:  aurMaintainer(ghOwner),
			Depends:     aurDeps,
			OptDepends:  aurOpt,
			Notice:      channelNotice(cfg, "aur"),
			Sources:     aurSources(archs, ghOwner, ghRepo, cfg.Project, version),
		}
	}
	reqs := aurRequirements(tagMissing)

	if !flagYes {
		archs, aerr := frozenArtifacts(fz, func() ([]build.Artifact, error) {
			return previewArchives(ctx, root, cfg, in, configPath, version)
		})
		if aerr != nil {
			return buildErrorResult(c, aerr)
		}
		ai := buildInput(archs)
		item := channel.PlanItem{
			Channel: "aur", Kind: channel.KindOwned,
			OwnedArtifact: "aur:" + pkg, Action: channel.ActionCreate,
			Diff: channel.Diff("", channel.GeneratePKGBUILD(ai)),
		}
		msg := "plan: push PKGBUILD for " + pkg
		msg += previewNote(version, tagMissing, true)
		res := output.New(c.Name, msg, true)
		res.Data = publishData{Applied: false, Plan: []channel.PlanItem{item}, Requires: reqs}
		res.Next = dryRunNext(item, reqs, "aur")
		return res
	}

	if tagMissing {
		return tagMissingResult(c, version)
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return tokenMissingResult(c)
	}
	sshKey := os.Getenv("AUR_SSH_KEY")
	if sshKey == "" {
		res := output.New(c.Name, "cannot publish without an AUR SSH key", false)
		res.Errors = []output.Problem{{Code: output.ErrTokenMissing, Message: "AUR_SSH_KEY required to push to AUR", Hint: "export AUR_SSH_KEY=\"$(cat ~/.ssh/aur)\""}}
		res.Next = []output.NextDo{{Reason: "set the key then retry", Do: "export AUR_SSH_KEY=… ; wharfy publish aur --yes"}}
		return res
	}
	// 実 release: linux tarball を GitHub Releases へ上げ、実 sha256 を得る。
	// BYO-binary(依頼①)は GoReleaser を通さず、記録済み成果物の再利用 or ネイティブ upload。
	// 凍結中は release を走らせず、その版を配ったときの記録をそのまま使う。
	archs, rerr := frozenArtifacts(fz, func() ([]build.Artifact, error) {
		a, _, err := releaseArtifacts(ctx, root, configPath, cfg, in, version)
		return a, err
	})
	if rerr != nil {
		return buildErrorResult(c, rerr)
	}
	ai := buildInput(archs)
	commit, perr := newAurPusher(sshKey).Push(ctx, pkg, ai.Files())
	if perr != nil {
		res := output.New(c.Name, "aur push failed", false)
		res.Errors = []output.Problem{{Code: output.ErrPublishFailed, Message: perr.Error(), Hint: "check AUR_SSH_KEY and that the package exists/you are a maintainer"}}
		res.Next = []output.NextDo{{Reason: "fix the cause then retry", Do: "wharfy publish aur --yes"}}
		return res
	}
	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		now := nowUTC().Format(time.RFC3339)
		if fz == nil { // 凍結中は release を走らせていない
			st.Publish["releases"] = state.PublishRecord{Version: version, Target: cfg.Github, At: now}
		}
		st.Publish["aur"] = state.PublishRecord{Version: version, Target: pkg, Commit: commit, At: now, Artifacts: archs}
		_ = state.Save(root, st)
	}
	item := channel.PlanItem{Channel: "aur", Kind: channel.KindOwned, OwnedArtifact: "aur:" + pkg, Action: channel.ActionUpdate}
	res := publishResult(c, "published "+pkg+" "+version+" → AUR", true, []channel.PlanItem{item})
	res.Data = publishData{Applied: true, Plan: []channel.PlanItem{item}}
	res.Next = []output.NextDo{
		{Reason: "users install with", Do: "yay -S " + pkg},
		{Reason: "verify install works", Do: "wharfy verify"},
	}
	return res
}

// aurSources は linux archive を AUR の source(URL+sha256)にする。
func aurSources(archs []build.Artifact, ghOwner, ghRepo, project, version string) []channel.AurSource {
	var out []channel.AurSource
	for _, a := range archs {
		if a.OS != "linux" {
			continue
		}
		if !strings.HasSuffix(a.Path, ".tar.gz") {
			continue // deb/rpm/appimage は AUR の source archive ではない(formula と同じ sha 汚染を避ける)
		}
		name := fmt.Sprintf("%s_%s_linux_%s.tar.gz", project, version, a.Arch)
		url := fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", ghOwner, ghRepo, version, name)
		out = append(out, channel.AurSource{Arch: a.Arch, URL: url, SHA256: a.SHA256})
	}
	return out
}

func aurMaintainer(owner string) string {
	if owner == "" {
		return ""
	}
	return owner + " <" + owner + "@users.noreply.github.com>"
}

// aurRequirements は aur の前提(tag / GITHUB_TOKEN(release) / AUR_SSH_KEY(push))。
func aurRequirements(tagMissing bool) []requirement {
	return []requirement{
		{Requirement: "git tag", Met: !tagMissing, Hint: "git tag vX.Y.Z && git push --tags (the tag is the version)"},
		{Requirement: "GITHUB_TOKEN", Met: os.Getenv("GITHUB_TOKEN") != "", Hint: "export GITHUB_TOKEN=… (upload the release tarball)"},
		{Requirement: "AUR_SSH_KEY", Met: os.Getenv("AUR_SSH_KEY") != "", Hint: "export AUR_SSH_KEY=\"$(cat ~/.ssh/aur)\""},
	}
}

// publishWinget は winget チャネル(gated)。manifest 3 種を生成し、microsoft/winget-pkgs を
// fork→branch→commit→PR まで組み立てる(マージはしない)。書く前に申請物を見せる。
// openGatedPR は gated チャネル(winget / homebrew-core)に既存の OPEN な PR があるかを確認し、
// あれば「重複 PR を出さない」ための Result を返す(無ければ nil = 提出してよい)。
// 記録に PR URL があれば live(GitHub API)で状態を確かめ、open のときだけガードする。
// 旧バージョンの PR が merge/close 済みなら、新バージョンの PR は出してよい。
// probe 不能のときは記録の state にフォールバックし、pr_open のときだけ安全側でガードする。
func openGatedPR(ctx context.Context, c registry.Command, root, project, chName string) *output.Result {
	st, err := state.Load(root, project)
	if err != nil {
		return nil
	}
	rec, ok := st.Publish[chName]
	if !ok || rec.PR == "" {
		return nil // 記録なし → 初回提出
	}
	live, perr := (&channel.WingetProbe{Token: os.Getenv("GITHUB_TOKEN"), API: wingetProbeBase}).ProbePR(ctx, rec.PR)
	switch {
	case perr == nil && live != "pr_open":
		return nil // merged/closed → 新しい PR を出してよい
	case perr != nil && rec.State != "pr_open":
		return nil // probe 不能かつ記録も open でない → ガードしない(提出を許可)
	}
	// open(または probe 不能で記録が open)→ 重複 PR を出さない。
	res := output.New(c.Name, chName+" PR already open — not opening a duplicate: "+rec.PR, true)
	res.Data = publishData{Applied: false, Plan: []channel.PlanItem{{
		Channel: chName, Kind: channel.KindGated, Action: channel.ActionSkip,
		Reason: "an earlier PR (" + rec.Version + ") is still under review",
	}}}
	res.Warnings = []output.Warning{{Code: output.WarnGatedPending, Message: chName + " PR awaiting review: " + rec.PR + " — merge or close it before submitting a new version"}}
	res.Next = []output.NextDo{
		{Reason: "track the open review (merge is the reviewer's call)", Do: "open " + rec.PR},
		{Reason: "check overall state", Do: "wharfy status"},
	}
	return &res
}

func publishWinget(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool) output.Result {
	identifier := channelTargetByName(cfg, "winget")
	ghOwner, ghRepo, ghOK := splitOwnerName(cfg.Github)
	if identifier == "" || !ghOK {
		item := channel.PlanItem{Channel: "winget", Kind: channel.KindGated, Action: channel.ActionSkip,
			Reason: "winget identifier/github unresolved — set 'github' or 'winget.identifier'"}
		res := publishResult(c, "winget skipped — unresolved", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "check the resolved config", Do: "wharfy config"}}
		return res
	}

	// BYO-bundle(GUI・依頼③)は goreleaser を通さない(main が無く生成不可)。configPath は
	// 非 bundle の release/preview 経路だけが使う。
	var configPath string
	if !cfg.Bundle {
		var err error
		configPath, err = writeGeneratedConfig(root, cfg, in, version)
		if err != nil {
			return internalError(c, err)
		}
	}
	buildInput := func(archs []build.Artifact) channel.WingetInput {
		return channel.WingetInput{
			Identifier:  identifier,
			Project:     cfg.Project,
			Version:     version,
			License:     cfg.License,
			Description: in.Description,
			Homepage:    cfg.Homepage,
			Installers:  wingetInstallersFor(cfg, archs, ghOwner, ghRepo, version),
		}
	}
	reqs := applyRequirements(tagMissing)

	if !flagYes {
		archs, aerr := previewArchives(ctx, root, cfg, in, configPath, version)
		if aerr != nil {
			return buildErrorResult(c, aerr)
		}
		wi := buildInput(archs)
		if len(wi.Installers) == 0 {
			// 配線対象(portable zip)が無い。Installers:[] の壊れた申請を予告せず skip する(scoop §1 と同型)。
			return gatedUnwiredSkip(c, "winget", channel.WingetUnwiredReason)
		}
		files := channel.GenerateWingetManifests(wi)
		item := channel.PlanItem{
			Channel: "winget", Kind: channel.KindGated,
			OwnedArtifact: "microsoft/winget-pkgs (PR from fork)",
			Action:        channel.ActionPrepare, Diff: manifestsDiff(files),
		}
		msg := "plan: prepare winget PR for " + identifier
		msg += previewNote(version, tagMissing, true)
		res := output.New(c.Name, msg, true)
		res.Data = publishData{Applied: false, Plan: []channel.PlanItem{item}, Requires: reqs}
		res.Next = dryRunNext(item, reqs, "winget")
		return res
	}

	if tagMissing {
		return tagMissingResult(c, version)
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return tokenMissingResult(c)
	}
	// 既存の OPEN な PR があれば二重に出さない(前回審査が未完了のとき)。
	if guard := openGatedPR(ctx, c, root, cfg.Project, "winget"); guard != nil {
		return *guard
	}
	// 実 release: windows zip を GitHub Releases へ上げ、実 sha256 を得る(installer が参照)。
	// 他の単体 publish と同じ合流点を通す — 記録済み成果物を再利用し、release を内包したときは
	// latest.json も出す(BYO-bundle は再 archive せず持ち込み zip をそのまま上げる)。
	archs, _, rerr := releaseArtifacts(ctx, root, configPath, cfg, in, version)
	if rerr != nil {
		return buildErrorResult(c, rerr)
	}
	wi := buildInput(archs)
	if len(wi.Installers) == 0 {
		// 配線対象(portable zip)が無い。Installers:[] の壊れた申請を PR せず skip する(scoop §1 と同型)。
		return gatedUnwiredSkip(c, "winget", channel.WingetUnwiredReason)
	}
	files := channel.GenerateWingetManifests(wi)
	prURL, serr := newWingetSubmitter(token).Submit(ctx, wi, files)
	if serr != nil {
		res := output.New(c.Name, "winget submission failed", false)
		res.Errors = []output.Problem{{Code: output.ErrPublishFailed, Message: serr.Error(), Hint: "check GITHUB_TOKEN scope (fork + PR)"}}
		res.Next = []output.NextDo{{Reason: "fix the cause then retry", Do: "wharfy publish winget --yes"}}
		return res
	}

	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		now := nowUTC().Format(time.RFC3339)
		st.Publish["releases"] = state.PublishRecord{Version: version, Target: cfg.Github, At: now}
		st.Publish["winget"] = state.PublishRecord{Version: version, Target: identifier, State: "pr_open", PR: prURL, At: now}
		_ = state.Save(root, st)
	}

	item := channel.PlanItem{Channel: "winget", Kind: channel.KindGated, OwnedArtifact: "microsoft/winget-pkgs (PR)", Action: channel.ActionPrepare}
	res := publishResult(c, "winget PR opened: "+prURL, true, []channel.PlanItem{item})
	res.Data = publishData{Applied: true, Plan: []channel.PlanItem{item}}
	res.Warnings = []output.Warning{{Code: output.WarnGatedPending, Message: "winget PR awaiting review (wharfy does not merge)"}}
	res.Next = []output.NextDo{
		{Reason: "track the review (merge is the reviewer's call)", Do: "open " + prURL},
		{Reason: "check overall state", Do: "wharfy status"},
	}
	return res
}

// wingetInstallers は windows archive を winget の installer(URL+sha256)にする。
func wingetInstallers(archs []build.Artifact, ghOwner, ghRepo, project, version string) []channel.WingetInstaller {
	var out []channel.WingetInstaller
	for _, a := range archs {
		if a.OS != "windows" {
			continue
		}
		name := fmt.Sprintf("%s_%s_windows_%s.zip", project, version, a.Arch)
		url := fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", ghOwner, ghRepo, version, name)
		out = append(out, channel.WingetInstaller{Arch: a.Arch, URL: url, SHA256: a.SHA256})
	}
	return out
}

// wingetBundleInstallers は BYO-bundle(GUI・依頼③)の windows zip(ポータブル: 中に <App>.exe)を
// installer にする。アセット名は持ち込みファイル名そのまま(release と一致)。exe/msi インストーラ種別は
// InstallerType が異なるため現状スコープ外(ポータブル zip を主経路にする)。
func wingetBundleInstallers(archs []build.Artifact, ghOwner, ghRepo, version string) []channel.WingetInstaller {
	var out []channel.WingetInstaller
	for _, a := range archs {
		if a.OS != "windows" || a.Kind != "zip" {
			continue
		}
		name := filepath.Base(a.Path)
		url := fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", ghOwner, ghRepo, version, name)
		out = append(out, channel.WingetInstaller{Arch: a.Arch, URL: url, SHA256: a.SHA256})
	}
	return out
}

// wingetInstallersFor は build/bundle モードに応じて winget installer リストを選ぶ。
func wingetInstallersFor(cfg config.Config, archs []build.Artifact, ghOwner, ghRepo, version string) []channel.WingetInstaller {
	if cfg.Bundle {
		return wingetBundleInstallers(archs, ghOwner, ghRepo, version)
	}
	return wingetInstallers(archs, ghOwner, ghRepo, cfg.Project, version)
}

// manifestsDiff は申請する manifest 3 種をファイル名つきで連結して見せる。
func manifestsDiff(files map[string]string) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString("--- " + n + " ---\n")
		b.WriteString(files[n])
		b.WriteString("\n")
	}
	return b.String()
}

// strictGated はコミュニティ審査が厳しい gated チャネル(誤申請がメンテナ負荷になる)。
// 申請前に基準提示＋明示同意(--acknowledge-review)を要求し、未同意では出さない。
// winget は低ハードルの正規自己申請ルートなので含めない(現状維持)。
var strictGated = map[string]struct{ criteria, etiquette string }{
	"homebrew-core": {
		criteria:  "homebrew-core requires a notable, established project AND a formula that passes `brew audit --new --strict`",
		etiquette: "this opens a PR a Homebrew maintainer must review — submit only if you genuinely qualify",
	},
}

// sourceTarballURL は tag のソース tarball(GitHub の自動 archive)を返す(core formula が参照)。
func sourceTarballURL(ghOwner, ghRepo, version string) string {
	return fmt.Sprintf("https://github.com/%s/%s/archive/refs/tags/v%s.tar.gz", ghOwner, ghRepo, version)
}

// sourceTarballSHA はソース tarball をダウンロードして sha256 を計算する(テストで差し替え)。
var sourceTarballSHA = func(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch source tarball %s: %s", url, resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// publishHomebrewCore は homebrew-core チャネル(strict gated・*-core)。core は notability ＋
// ソースビルド formula ＋ 厳格審査が要る。wharfy は **source-build formula** を生成して fork PR を
// 組むが、(1) 受け入れ基準を提示し (2) --acknowledge-review が無ければ出さない(コミュニティ配慮)。
// マージはしない。出すのはあくまで叩き台で brew audit 合格保証ではない。
func publishHomebrewCore(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool) output.Result {
	central := channelTargetByName(cfg, "homebrew-core")
	ghOwner, ghRepo, ghOK := splitOwnerName(cfg.Github)
	if central == "" || !ghOK {
		item := channel.PlanItem{Channel: "homebrew-core", Kind: channel.KindGated, Action: channel.ActionSkip,
			Reason: "github unresolved — formula needs the source repo"}
		res := publishResult(c, "homebrew-core skipped — unresolved", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "check the resolved config", Do: "wharfy config"}}
		return res
	}
	crit := strictGated["homebrew-core"]
	formulaFile := channel.CoreFormulaPath(cfg.Project)
	srcURL := sourceTarballURL(ghOwner, ghRepo, version)
	mkFormula := func(sha string) string {
		return channel.GenerateCoreFormula(channel.CoreFormulaInput{
			Project: cfg.Project, Binary: cfg.Project, Main: cfg.Main, Description: in.Description,
			Homepage: cfg.Homepage, License: cfg.License, Version: version, Dependencies: homebrewDeps(in),
			SourceURL: srcURL, SourceSHA: sha,
		})
	}
	// strict gated は tag/token に加え「明示同意(--acknowledge-review)」を要件に出す。
	reqs := []requirement{
		{Requirement: "git tag", Met: !tagMissing, Hint: "git tag vX.Y.Z && git push --tags (the tag is the version)"},
		{Requirement: "GITHUB_TOKEN", Met: os.Getenv("GITHUB_TOKEN") != "", Hint: "export GITHUB_TOKEN=… (fork + PR)"},
		{Requirement: "acknowledge-review", Met: flagAckReview, Hint: "pass --acknowledge-review after confirming you meet the criteria"},
	}

	if !flagYes {
		item := channel.PlanItem{
			Channel: "homebrew-core", Kind: channel.KindGated,
			OwnedArtifact: central + " (PR from fork): " + formulaFile,
			Action:        channel.ActionPrepare,
			Diff:          channel.Diff("", mkFormula("")),
		}
		msg := "plan: prepare homebrew-core PR for " + cfg.Project + previewNote(version, tagMissing, true)
		res := output.New(c.Name, msg, true)
		res.Data = publishData{Applied: false, Plan: []channel.PlanItem{item}, Requires: reqs}
		// コミュニティ負荷の正直なゲート: 基準と作法を先に見せる。
		res.Warnings = []output.Warning{{Code: output.WarnGatedPending, Message: crit.criteria + "; " + crit.etiquette}}
		next := []output.NextDo{{Reason: "confirm it passes review locally first", Do: "brew audit --new --strict " + cfg.Project}}
		for _, r := range reqs {
			if !r.Met {
				next = append(next, output.NextDo{Reason: "required before --yes: " + r.Requirement, Do: r.Hint})
			}
		}
		// 申請コマンドは --acknowledge-review を含めて正確に示す(strict gated)。
		res.Next = append(next, output.NextDo{Reason: "acknowledge the criteria and submit", Do: "wharfy publish homebrew-core --yes --acknowledge-review"})
		return res
	}

	if tagMissing {
		return tagMissingResult(c, version)
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return tokenMissingResult(c)
	}
	// 明示同意が無ければ出さない(strict gated のフットガン防止・コミュニティ配慮)。
	if !flagAckReview {
		res := output.New(c.Name, "homebrew-core needs explicit acknowledgement", false)
		res.Errors = []output.Problem{{Code: output.ErrConsentRequired, Message: crit.criteria,
			Hint: crit.etiquette + ". Re-run with --acknowledge-review once `brew audit --new --strict` passes."}}
		res.Next = []output.NextDo{
			{Reason: "confirm it passes review locally", Do: "brew audit --new --strict " + cfg.Project},
			{Reason: "acknowledge the criteria and submit", Do: "wharfy publish homebrew-core --yes --acknowledge-review"},
		}
		return res
	}
	// 既存の OPEN な PR があれば二重に出さない(前回審査が未完了のとき)。
	if guard := openGatedPR(ctx, c, root, cfg.Project, "homebrew-core"); guard != nil {
		return *guard
	}
	// source-build formula は tag のソース tarball を参照する。その実 sha を計算する(release 不要)。
	sha, err := sourceTarballSHA(ctx, srcURL)
	if err != nil {
		res := output.New(c.Name, "could not fetch the source tarball", false)
		res.Errors = []output.Problem{{Code: output.ErrNetworkError, Message: err.Error(), Hint: "ensure the tag is pushed to GitHub, then retry"}}
		res.Next = []output.NextDo{{Reason: "push the tag then retry", Do: "git push --tags ; wharfy publish homebrew-core --yes --acknowledge-review"}}
		return res
	}
	prURL, serr := newCoreSubmitter(token).Submit(ctx, channel.CoreInput{
		Central: central, Project: cfg.Project, Version: version,
		FormulaFile: formulaFile, Formula: mkFormula(sha),
	})
	if serr != nil {
		res := output.New(c.Name, "homebrew-core submission failed", false)
		res.Errors = []output.Problem{{Code: output.ErrPublishFailed, Message: serr.Error(), Hint: "check GITHUB_TOKEN scope (fork + PR)"}}
		res.Next = []output.NextDo{{Reason: "fix the cause then retry", Do: "wharfy publish homebrew-core --yes --acknowledge-review"}}
		return res
	}

	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		st.Publish["homebrew-core"] = state.PublishRecord{Version: version, Target: central, State: "pr_open", PR: prURL, At: nowUTC().Format(time.RFC3339)}
		_ = state.Save(root, st)
	}

	item := channel.PlanItem{Channel: "homebrew-core", Kind: channel.KindGated, OwnedArtifact: central + " (PR)", Action: channel.ActionPrepare}
	res := publishResult(c, "homebrew-core PR opened: "+prURL, true, []channel.PlanItem{item})
	res.Data = publishData{Applied: true, Plan: []channel.PlanItem{item}}
	res.Warnings = []output.Warning{{Code: output.WarnGatedPending, Message: "homebrew-core PR awaiting review (wharfy does not merge; brew audit is yours)"}}
	res.Next = []output.NextDo{
		{Reason: "track the review (merge is the maintainer's call)", Do: "open " + prURL},
		{Reason: "check overall state", Do: "wharfy status"},
	}
	return res
}

// ghcrHost は GITHUB_TOKEN で開くレジストリ。既定のイメージ名もここを指す(config/resolve.go)。
const ghcrHost = "ghcr.io"

// loginToImageRegistry は push 先レジストリへログインする(push の手前で必ず通る)。
// CI の docker には資格情報が無いのが普通で、トークンを渡しただけでは push が 401 で落ちていた
// ——要件は GITHUB_TOKEN を「要る」と言うのに、認証には誰も使っていなかった。ここを wharfy が
// 担うことで「シークレットを登録すれば動く」が本当になる。
// ghcr 以外のレジストリは GITHUB_TOKEN では開かないので、手元/CI 側の資格情報に任せる(no-op)。
func loginToImageRegistry(ctx context.Context, cfg config.Config) error {
	image := channelTargetByName(cfg, "container")
	if image == "" || build.RegistryHost(image) != ghcrHost {
		return nil
	}
	owner, _, _ := splitOwnerName(cfg.Github)
	return newRegistryLogin().Login(ctx, ghcrHost, owner, os.Getenv("GITHUB_TOKEN"))
}

// publishContainer は container チャネル(ghcr OCI・マルチアーキ)。goreleaser の
// docker pipe で per-arch イメージをビルドし ghcr へ push、manifest list を作る。
// docker デーモンが要る。ghcr へのログインは wharfy が GITHUB_TOKEN(packages:write)で行う。
// 書く前に計画を見せる。
func publishContainer(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool) output.Result {
	image := channelTargetByName(cfg, "container")
	if image == "" {
		item := channel.PlanItem{Channel: "container", Kind: channel.KindOwned, Action: channel.ActionSkip,
			Reason: "container image unresolved — set 'github' or 'container.image'"}
		res := publishResult(c, "container skipped — image unresolved", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "check the resolved config", Do: "wharfy config"}}
		return res
	}

	reqs := containerRequirements(tagMissing)
	item := channel.PlanItem{
		Channel: "container", Kind: channel.KindOwned, OwnedArtifact: image,
		Action: channel.ActionCreate, Diff: containerDiff(cfg, image, version),
	}

	if !flagYes {
		msg := "plan: build+push " + image + " (multi-arch OCI)"
		if tagMissing {
			msg += " (preview @ " + version + "; no git tag yet)"
		}
		res := output.New(c.Name, msg, true)
		res.Data = publishData{Applied: false, Plan: []channel.PlanItem{item}, Requires: reqs}
		res.Next = dryRunNext(item, reqs, "container")
		return res
	}

	if tagMissing {
		return tagMissingResult(c, version)
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return tokenMissingResult(c)
	}
	if !dockerAvailable() {
		res := output.New(c.Name, "cannot publish: docker is unavailable", false)
		res.Errors = []output.Problem{{Code: output.ErrBuilderUnavailable, Message: "docker CLI not found", Hint: "install Docker (with buildx) and start the daemon"}}
		res.Next = []output.NextDo{{Reason: "install docker then retry", Do: "wharfy publish container --yes"}}
		return res
	}

	if lerr := loginToImageRegistry(ctx, cfg); lerr != nil {
		return buildErrorResult(c, lerr)
	}

	// BYO-binary(依頼① #3): buildx で持ち込みバイナリからマルチアーキ OCI を build+push。
	if cfg.Prebuilt {
		if cerr := newPrebuiltContainerizer().PushMultiArch(ctx, root, config.DistDir, image, version, prebuiltBinaryName(cfg, in), toPrebuiltBinaries(in)); cerr != nil {
			return buildErrorResult(c, cerr)
		}
	} else {
		configPath, err := writeGeneratedConfig(root, cfg, in, version)
		if err != nil {
			return internalError(c, err)
		}
		if _, cerr := newContainerizer(config.DistDir).Containers(ctx, root, configPath); cerr != nil {
			return buildErrorResult(c, cerr)
		}
	}
	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		st.Publish["container"] = state.PublishRecord{Version: version, Target: image, At: nowUTC().Format(time.RFC3339)}
		_ = state.Save(root, st)
	}
	item.Action = channel.ActionUpdate
	res := publishResult(c, "published "+image+":"+version+" (multi-arch)", true, []channel.PlanItem{item})
	res.Data = publishData{Applied: true, Plan: []channel.PlanItem{item}}
	res.Next = []output.NextDo{
		{Reason: "users pull with", Do: "docker pull " + image + ":" + version},
		{Reason: "verify install works", Do: "wharfy verify"},
	}
	return res
}

func containerDiff(cfg config.Config, image, version string) string {
	arches := config.DefaultGOARCH
	if cfg.Build != nil && len(cfg.Build.GOARCH) > 0 {
		arches = cfg.Build.GOARCH
	}
	var b strings.Builder
	for _, a := range arches {
		b.WriteString("+ " + image + ":" + version + "-" + a + "\n")
	}
	b.WriteString("→ " + image + ":" + version + ", " + image + ":latest (manifest list)\n")
	return b.String()
}

// containerRequirements は container の前提(tag / GITHUB_TOKEN(ghcr) / docker)。
func containerRequirements(tagMissing bool) []requirement {
	return []requirement{
		{Requirement: "git tag", Met: !tagMissing, Hint: "git tag vX.Y.Z && git push --tags (the tag is the version)"},
		{Requirement: "GITHUB_TOKEN", Met: os.Getenv("GITHUB_TOKEN") != "", Hint: "export GITHUB_TOKEN=… (ghcr packages:write)"},
		{Requirement: "docker", Met: dockerAvailable(), Hint: "install Docker (with buildx) and start the daemon"},
	}
}

// publishLinuxPkg は apt(deb)/rpm チャネル。nfpm で deb/rpm を生成し、hosted repo へ
// multipart POST でアップロードする(PACKAGE_REPO_TOKEN。GitHub には触れない)。
// repo 未設定は skip して案内(channel_skipped)。プロバイダ依存のため `-F package=@` 形を既定。
func publishLinuxPkg(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool, chName, ext string) output.Result {
	repo := channelTargetByName(cfg, chName)        // 配信 URL(probe/install/表示)
	pushURL := channelPushTargetByName(cfg, chName) // アップロード先(fury は別ホスト)
	if pushURL == "" {
		item := channel.PlanItem{Channel: chName, Kind: channel.KindOwned, Action: channel.ActionSkip,
			Reason: chName + " has no hosted repo configured"}
		res := publishResult(c, chName+" skipped — no hosted repo configured", true, []channel.PlanItem{item})
		res.Warnings = []output.Warning{{Code: output.WarnChannelSkipped, Message: chName + " skipped — choose a host (see next:)"}}
		res.Next = pkgHostingGuide(chName)
		return res
	}
	if repo == "" {
		repo = pushURL // 生 push のみ指定された場合は表示・記録に流用
	}

	// preview のパッケージ名。BYO 併用時は CLI(prebuilt)＋GUI(bundle)の両方を出す(依頼②)。
	var names []string
	if cfg.Prebuilt || cfg.Bundle {
		if cfg.Prebuilt {
			names = append(names, expectedPackages(cfg, version, ext)...)
		}
		if cfg.Bundle {
			// BYO-bundle(GUI・依頼③): deb/rpm は各アプリが持ち込む。パッケージ名(<app>-app)は
			// バンドラ生成物に焼き込まれており、wharfy は名前を付け替えずそのまま同じ hosted repo に上げる。
			names = append(names, bundlePackageNames(in, ext)...)
		}
	} else {
		names = expectedPackages(cfg, version, ext)
	}
	item := channel.PlanItem{
		Channel: chName, Kind: channel.KindOwned, OwnedArtifact: repo,
		Action: channel.ActionCreate, Diff: packageDiff(names, repo),
	}
	reqs := pkgRequirements(tagMissing)

	if !flagYes {
		msg := "plan: upload " + strconv.Itoa(len(names)) + " " + ext[1:] + " package(s) → " + repo
		if tagMissing {
			msg += " (preview @ " + version + "; no git tag yet)"
		}
		res := output.New(c.Name, msg, true)
		res.Data = publishData{Applied: false, Plan: []channel.PlanItem{item}, Requires: reqs}
		res.Next = dryRunNext(item, reqs, chName)
		return res
	}

	if tagMissing {
		return tagMissingResult(c, version)
	}
	token := resolveToken("PACKAGE_REPO_TOKEN", "package_repo_token")
	if token == "" {
		res := output.New(c.Name, "cannot publish without a token", false)
		res.Errors = []output.Problem{{Code: output.ErrTokenMissing, Message: "PACKAGE_REPO_TOKEN required to upload to the hosted repo", Hint: "export PACKAGE_REPO_TOKEN=… or run: wharfy auth fury"}}
		res.Next = []output.NextDo{
			{Reason: "save the token to the OS keychain, then retry", Do: "wharfy auth fury"},
			{Reason: "or pass it via env then retry", Do: "export PACKAGE_REPO_TOKEN=… ; wharfy publish " + chName + " --yes"},
		}
		return res
	}

	// BYO-binary(依頼① #3): 持ち込みバイナリから nfpm で deb/rpm を作る(GoReleaser を通さない)。
	// BYO-bundle(依頼③): 持ち込みの deb/rpm をそのまま使う(生成しない・ext フィルタで該当分だけ上げる)。
	var pkgs []build.Artifact
	if cfg.Prebuilt || cfg.Bundle {
		// 併用時は両方から該当拡張子(.deb/.rpm)を集める(依頼②)。prebuilt=CLI パッケージ(nfpm)、
		// bundle=持ち込み GUI パッケージ。以降の ext フィルタで該当分だけ hosted repo に上げる。
		if cfg.Prebuilt {
			p, perr := build.PackagePrebuilt(root, config.DistDir, prebuiltPackageSpec(cfg, in, chName, ext, version), toPrebuiltBinaries(in))
			if perr != nil {
				return buildErrorResult(c, perr)
			}
			pkgs = append(pkgs, p...)
		}
		if cfg.Bundle {
			p, perr := build.ValidateBundles(root, toBundles(in))
			if perr != nil {
				return buildErrorResult(c, perr)
			}
			pkgs = append(pkgs, p...)
		}
	} else {
		configPath, err := writeGeneratedConfig(root, cfg, in, version)
		if err != nil {
			return internalError(c, err)
		}
		p, perr := newPackager(config.DistDir).Packages(ctx, root, configPath)
		if perr != nil {
			return buildErrorResult(c, perr)
		}
		pkgs = p
	}
	uploaded := 0
	for _, p := range pkgs {
		if filepath.Ext(p.Path) != ext {
			continue
		}
		full := p.Path
		if !filepath.IsAbs(full) {
			full = filepath.Join(root, p.Path)
		}
		if uerr := uploadPackage(ctx, pushURL, token, full); uerr != nil {
			res := output.New(c.Name, "publish failed", false)
			res.Errors = []output.Problem{{Code: output.ErrPublishFailed, Message: uerr.Error(), Hint: "check PACKAGE_REPO_TOKEN scope and the repo URL"}}
			res.Next = []output.NextDo{{Reason: "fix the cause then retry", Do: "wharfy publish " + chName + " --yes"}}
			return res
		}
		uploaded++
	}
	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		st.Publish[chName] = state.PublishRecord{Version: version, Target: repo, At: nowUTC().Format(time.RFC3339)}
		_ = state.Save(root, st)
	}
	item.Action = channel.ActionUpdate
	res := publishResult(c, "published "+strconv.Itoa(uploaded)+" "+ext[1:]+" package(s) → "+repo, true, []channel.PlanItem{item})
	res.Data = publishData{Applied: true, Plan: []channel.PlanItem{item}}
	res.Next = []output.NextDo{{Reason: "install from the channel and run it", Do: "wharfy verify"}}
	if w := pkgNotIndexedWarning(ctx, chName, repo, cfg.Project, version); w != nil {
		res.Warnings = append(res.Warnings, *w)
		res.Next = append([]output.NextDo{{
			Reason: "confirm consumers can actually install it (the upload succeeded; the public index is what they read)",
			Do:     "wharfy verify " + chName,
		}}, res.Next...)
	}
	return res
}

var (
	// pkgIndexTimeout は publish 直後の索引確認 1 本の上限。配布の成否を左右しないので短く切る。
	pkgIndexTimeout = 15 * time.Second
	// checkPkgIndex は公開索引を引く末端(テストで差し替え)。nil なら索引確認をしない
	// —— テストは既定でこれを nil にして、実 repo へネットワークを飛ばさない。
	checkPkgIndex = probeLinuxRepo
)

// pkgNotIndexedWarning は hosted repo(apt/rpm)へ上げた版が、公開リポジトリの索引に現れたかを
// 確かめ、現れていなければ警告を返す(現れていれば nil)。
//
// アップロードが 200 を返せば publish は「✓ published」と言い切る —— 嘘ではないが、利用者はまだ
// 誰も入れられないことがある。fury 系は受け取ったパッケージを**既定で非公開**として扱い、
// ダッシュボードで公開に切り替えるまで公開 repo(apt.fury.io/<user>/)に載せない。初回に必ず踏む穴で、
// しかも配布者からは見えない。索引の生成にも数分かかるので、ここで「まだ無い」ことは失敗ではない
// ——だから ok は落とさず、両方の可能性を名指しして次の一手を渡す。
func pkgNotIndexedWarning(ctx context.Context, chName, repo, pkg, version string) *output.Warning {
	if repo == "" || checkPkgIndex == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pkgIndexTimeout)
	defer cancel()
	rs, err := checkPkgIndex(ctx, chName, repo, pkg)
	if err == nil && rs.Found && rs.Version == version {
		return nil
	}
	return &output.Warning{
		Code: output.WarnPkgNotIndexed,
		Message: chName + " " + version + " was uploaded, but " + repo + " does not serve it yet — " +
			"hosted repos take a few minutes to index, and some (fury) receive uploads as private: " +
			"if it never appears, flip the package to public in the provider's dashboard",
	}
}

// pkgHostingGuide は apt/rpm の repo 未設定時に「どこにホストするか」を判断軸つきで案内する。
// 配布者はここで必ず選択を迫られるため、推奨順(fury → GitHub Pages → Releases)と設定例を示す。
func pkgHostingGuide(chName string) []output.NextDo {
	return []output.NextDo{
		{
			Reason: "recommended: fury.io — free for public, no bandwidth cap, signup once",
			Do:     "set '" + chName + ": {provider: fury, user: <name>}' in wharfy.yaml ; export PACKAGE_REPO_TOKEN=… ; wharfy publish " + chName + " --yes",
		},
		{
			Reason: "alternative: any hosted apt/rpm service — give delivery + upload URLs",
			Do:     "set '" + chName + ": {repo: <deliver-url>, push: <upload-url>}' in wharfy.yaml ; export PACKAGE_REPO_TOKEN=…",
		},
		{
			Reason: "no extra account (GitHub only) but 100GB/mo cap — GitHub Pages provider not yet implemented",
			Do:     "or attach the .deb/.rpm to GitHub Releases for manual install (no apt/rpm repo)",
		},
	}
}

// prebuiltPackageSpec は BYO-binary の deb/rpm 生成指定を cfg/in から組む(#3)。
func prebuiltPackageSpec(cfg config.Config, in config.File, chName, ext, version string) build.PackageSpec {
	format := "deb"
	if chName == "rpm" {
		format = "rpm"
	}
	depends, recommends, suggests := config.RepoDeps(chName, in)
	return build.PackageSpec{
		Format:      format,
		Ext:         ext,
		Name:        cfg.Project,
		BinaryName:  prebuiltBinaryName(cfg, in),
		Version:     version,
		Maintainer:  pkgMaintainer(cfg),
		Description: in.Description,
		Notice:      channelNotice(cfg, chName),
		Homepage:    cfg.Homepage,
		License:     cfg.License,
		Depends:     depends,
		Recommends:  recommends,
		Suggests:    suggests,
	}
}

// pkgMaintainer は deb が必須とする maintainer を github owner から組む(config.maintainer と同形)。
func pkgMaintainer(cfg config.Config) string {
	if owner, _, ok := splitOwnerName(cfg.Github); ok {
		return fmt.Sprintf("%s <%s@users.noreply.github.com>", owner, owner)
	}
	return "wharfy <noreply@wharfy.local>"
}

// expectedPackages は生成される deb/rpm のファイル名(linux × goarch)。dry-run で見せる。
func expectedPackages(cfg config.Config, version, ext string) []string {
	goarch := config.DefaultGOARCH
	if cfg.Build != nil && len(cfg.Build.GOARCH) > 0 {
		goarch = cfg.Build.GOARCH
	}
	var out []string
	for _, arch := range goarch {
		out = append(out, fmt.Sprintf("%s_%s_linux_%s%s", cfg.Project, version, arch, ext))
	}
	return out
}

// bundlePackageNames は BYO-bundle(GUI)で持ち込む deb/rpm の実ファイル名を返す(preview 表示用)。
// 命名はバンドラ生成物に従う(wharfy は付け替えない)ので、宣言パスの basename をそのまま出す。
func bundlePackageNames(in config.File, ext string) []string {
	if in.Bundle == nil {
		return nil
	}
	var out []string
	for _, b := range in.Bundle.Bundles {
		if filepath.Ext(b.Path) == ext {
			out = append(out, filepath.Base(b.Path))
		}
	}
	return out
}

func packageDiff(names []string, repo string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString("+ " + n + "\n")
	}
	b.WriteString("→ " + repo + "\n")
	return b.String()
}

// pkgRequirements は apt/rpm の前提(GitHub ではなく PACKAGE_REPO_TOKEN)。
func pkgRequirements(tagMissing bool) []requirement {
	return []requirement{
		{Requirement: "git tag", Met: !tagMissing, Hint: "git tag vX.Y.Z && git push --tags (the tag is the version)"},
		{Requirement: "PACKAGE_REPO_TOKEN", Met: resolveToken("PACKAGE_REPO_TOKEN", "package_repo_token") != "", Hint: "export PACKAGE_REPO_TOKEN=… or run: wharfy auth fury (keychain)"},
	}
}

// httpUploadPackage は hosted repo へ multipart POST する(field "package"=ファイル、
// 認証は basic auth で username=token。`curl -F package=@… -u token:` 相当)。
func httpUploadPackage(ctx context.Context, repoURL, token, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("package", filepath.Base(filePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, repoURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.SetBasicAuth(token, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload %s: %s: %s", filepath.Base(filePath), resp.Status, string(b[:min(len(b), 200)]))
	}
	return nil
}

// publishViaRelease は「archive をアップロードして所有リポジトリに manifest/formula を書く」
// owned チャネル共通の発行フロー(homebrew/scoop)。makePub が archive から Publisher を組む。
func publishViaRelease(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool, chName, target string, fz *channelFreeze, makePub func([]build.Artifact) channel.Publisher) output.Result {
	// 生成物(goreleaser.yaml ＋ script 有効なら install.sh)を .wharfy/ に書く。
	// BYO-bundle(GUI・依頼③)は goreleaser を通さない(main が無く生成不可)ため configPath は空。
	// 凍結中はビルドも release も走らないので、生成設定そのものが要らない(version も凍結版で、
	// install.sh に書き戻すと script チャネルの版を取り違える)。
	var configPath string
	if !cfg.Bundle && fz == nil {
		var err error
		configPath, err = writeGeneratedConfig(root, cfg, in, version)
		if err != nil {
			return internalError(c, err)
		}
	}

	if !flagYes {
		// preview: ローカルに archive を作り(アップロードしない)、暫定 sha で差分を見せる。
		archs, aerr := frozenArtifacts(fz, func() ([]build.Artifact, error) {
			return previewArchives(ctx, root, cfg, in, configPath, version)
		})
		if aerr != nil {
			return buildErrorResult(c, aerr)
		}
		return ownedReleaseDryRun(ctx, c, makePub(archs), version, chName, target, tagMissing)
	}

	// apply: 高コストな実リリースの前に前提を確認する(tag / token)。
	if tagMissing {
		return tagMissingResult(c, version)
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return tokenMissingResult(c)
	}
	// release が済んでいれば(同 version)再アップロードせず記録済み成果物を使う(工程の分離・c2)。
	// 無ければ実リリースを走らせて実 sha256 を得る(--skip=homebrew・後方互換のフォールバック)。
	// 凍結中は release を走らせず、その版を配ったときの記録をそのまま使う。
	archs, aerr := frozenArtifacts(fz, func() ([]build.Artifact, error) {
		a, _, err := releaseArtifacts(ctx, root, configPath, cfg, in, version)
		return a, err
	})
	if aerr != nil {
		return buildErrorResult(c, aerr)
	}
	pub := makePub(archs)
	// 書き込み前の自己検査(#10): manifest の sha が実アセットと食い違えば止める(#9 の多層防御)。
	if err := verifyManifestChecksums(pub, archs); err != nil {
		return checksumMismatchResult(c, chName, err)
	}
	return ownedReleaseApply(ctx, c, pub, root, cfg.Project, chName, target, cfg.Github, version, archs, fz)
}

// releaseArtifacts は publish の apply で使う成果物を返す。release(同 version)が記録済みなら
// 再アップロードせず再利用し(reused=true)、無ければ release パスを走らせて記録する(後方互換)。
// BYO-binary(依頼①)では GoReleaser を通さず、自前 archive＋ネイティブ Release upload を使う。
func releaseArtifacts(ctx context.Context, root, configPath string, cfg config.Config, in config.File, version string) ([]build.Artifact, bool, error) {
	if set, found, _ := build.LoadArtifacts(root); found && set.Version == version {
		return set.Artifacts, true, nil
	}
	var (
		archs []build.Artifact
		err   error
	)
	if cfg.Prebuilt || cfg.Bundle {
		archs, err = byoRelease(ctx, root, cfg, in, version) // 併用時は両方を出す(依頼②)
	} else {
		archs, err = newReleaser(config.DistDir).Release(ctx, root, configPath)
	}
	if err != nil {
		return nil, false, err
	}
	_ = build.SaveArtifacts(root, version, archs) // 後続の publish <ch> が再利用できるよう記録
	// release を内包した経路も latest.json を出す。runRelease だけが出していたため、release を
	// 独立に叩かず publish だけで配ると Release から latest.json が落ち、更新チェックが 404 を
	// 引いていた(v0.20.0)。実 release を走らせたここが、その経路の合流点になる。
	if err := uploadLatestJSON(ctx, root, cfg, version, archs); err != nil {
		return nil, false, err
	}
	return archs, false, nil
}

// previewArchives は dry-run 用にローカル archive を作り、暫定 sha を得る(アップロードしない)。
// BYO-binary では持ち込みバイナリをそのまま archive 化する(実 sha になる)。それ以外は
// GoReleaser の snapshot でローカル生成する。
func previewArchives(ctx context.Context, root string, cfg config.Config, in config.File, configPath, version string) ([]build.Artifact, error) {
	if cfg.Prebuilt || cfg.Bundle {
		// 併用時は両方の成果物を返す(依頼②)。dry-run なので prebuilt は署名前の暫定 sha。
		var archs []build.Artifact
		if cfg.Prebuilt {
			a, err := build.ArchivePrebuilt(root, config.DistDir, cfg.Project, version, prebuiltBinaryName(cfg, in), toPrebuiltBinaries(in))
			if err != nil {
				return nil, err
			}
			archs = append(archs, a...)
		}
		if cfg.Bundle {
			// BYO-bundle は再 archive しない — 持ち込みバンドルを検証して実 sha 付き成果物にする(依頼③)。
			a, err := build.ValidateBundles(root, toBundles(in))
			if err != nil {
				return nil, err
			}
			archs = append(archs, a...)
		}
		return archs, nil
	}
	return newArchiver(config.DistDir).Archives(ctx, root, configPath)
}

// prebuiltBinaryName は archive 内の実行ファイル名(既定は project、binary 明示で上書き)。
func prebuiltBinaryName(cfg config.Config, in config.File) string {
	if in.Binary != "" {
		return in.Binary
	}
	return cfg.Project
}

func ownedSkip(c registry.Command, chName, reason string) output.Result {
	item := channel.PlanItem{Channel: chName, Kind: channel.KindOwned, Action: channel.ActionSkip, Reason: reason}
	res := publishResult(c, chName+" skipped — unresolved target", true, []channel.PlanItem{item})
	res.Next = []output.NextDo{{Reason: "check the resolved config", Do: "wharfy config"}}
	return res
}

// ownedSkipItem は Publisher が返した skip(配線不能など)を surface する(依頼書七通目 §1)。
// 何も書かず・state に記録せず、理由を warning に載せて「壊れたマニフェストを黙って出さなかった」ことを配布者に見せる。
func ownedSkipItem(c registry.Command, chName string, item channel.PlanItem) output.Result {
	res := publishResult(c, chName+" skipped — nothing to wire", true, []channel.PlanItem{item})
	res.Warnings = []output.Warning{{Code: output.WarnChannelSkipped, Message: chName + " skipped: " + item.Reason}}
	res.Next = []output.NextDo{{Reason: "wire an artifact " + chName + " can install, or drop " + chName + " for this bundle", Do: "wharfy config"}}
	return res
}

// gatedUnwiredSkip は gated チャネル(winget 等)で配線対象が無いときの skip を surface する
// (scoop §1 と同型)。空 installer の壊れた申請を PR せず、理由を warning に載せて配布者に見せる。
func gatedUnwiredSkip(c registry.Command, chName, reason string) output.Result {
	item := channel.PlanItem{Channel: chName, Kind: channel.KindGated, Action: channel.ActionSkip, Reason: reason}
	res := publishResult(c, chName+" skipped — nothing to wire", true, []channel.PlanItem{item})
	res.Warnings = []output.Warning{{Code: output.WarnChannelSkipped, Message: chName + " skipped: " + reason}}
	res.Next = []output.NextDo{{Reason: "wire an artifact " + chName + " can install, or drop " + chName + " for this bundle", Do: "wharfy config"}}
	return res
}

// publishScript は script チャネル(curl|sh インストーラ)。install.sh を生成し、
// 実 release の extra_files で同梱アップロードする。書く前に install.sh の内容を見せる。
func publishScript(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool) output.Result {
	if cfg.Github == "" {
		item := channel.PlanItem{Channel: "script", Kind: channel.KindOwned, Action: channel.ActionSkip,
			Reason: "github unresolved — install.sh needs the release repo"}
		res := publishResult(c, "script skipped — github unresolved", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "set github so the release can be derived", Do: "wharfy config"}}
		return res
	}

	// 凍結(ship:false)なら install.sh が入れるのは最後に配った版(告知だけが新しくなる)。
	scriptVer, _ := installScriptTarget(root, cfg, version)
	script := config.GenerateInstallScript(cfg, scriptVer)
	item := channel.PlanItem{
		Channel: "script", Kind: channel.KindOwned,
		OwnedArtifact: cfg.Github + " release:" + config.InstallScriptName,
		Action:        channel.ActionCreate,
		Diff:          channel.Diff("", script), // 同梱する install.sh の内容を見せる
	}
	curl := "curl -fsSL " + config.InstallURL(cfg) + " | sh"

	if !flagYes {
		reqs := applyRequirements(tagMissing)
		msg := "plan: upload " + config.InstallScriptName + " to the release"
		if tagMissing {
			msg += " (preview @ " + version + "; no git tag yet)"
		}
		res := output.New(c.Name, msg, true)
		res.Data = publishData{Applied: false, Plan: []channel.PlanItem{item}, Requires: reqs}
		res.Next = dryRunNext(item, reqs, "script")
		return res
	}

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
	// 実 release: archive ＋ install.sh を GitHub Releases へアップロード。BYO(依頼①②)は
	// GoReleaser を通さず、記録済み成果物を再利用 or ネイティブ upload(releaseArtifacts が両対応)。
	if cfg.Prebuilt || cfg.Bundle {
		if _, _, rerr := releaseArtifacts(ctx, root, configPath, cfg, in, version); rerr != nil {
			return buildErrorResult(c, rerr)
		}
	} else if _, rerr := newReleaser(config.DistDir).Release(ctx, root, configPath); rerr != nil {
		return buildErrorResult(c, rerr)
	}
	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		now := nowUTC().Format(time.RFC3339)
		st.Publish["releases"] = state.PublishRecord{Version: version, Target: cfg.Github, At: now}
		// script が配るのは install.sh が入れる版(凍結中は旧版)。release の版とは別物。
		st.Publish["script"] = state.PublishRecord{Version: scriptVer, Target: cfg.Github + " release:" + config.InstallScriptName, At: now}
		_ = state.Save(root, st)
	}
	item.Action = channel.ActionUpdate
	res := publishResult(c, "published "+config.InstallScriptName+" → "+cfg.Github+" release", true, []channel.PlanItem{item})
	res.Data = publishData{Applied: true, Plan: []channel.PlanItem{item}}
	res.Next = []output.NextDo{
		{Reason: "users install with", Do: curl},
		{Reason: "verify install works", Do: "wharfy verify"},
	}
	return res
}

// publishGoinstall は goinstall チャネル(梱包ゼロ)。何も push せず、module proxy で
// go install 可否を確認して手順を案内する。--yes でも書き込みは無い(noop)。
func publishGoinstall(ctx context.Context, c registry.Command, root string, cfg config.Config, tagMissing bool) output.Result {
	mod := channelTargetByName(cfg, "goinstall")
	if mod == "" {
		item := channel.PlanItem{Channel: "goinstall", Kind: channel.KindOwned, Action: channel.ActionSkip,
			Reason: "module unresolved — needs a go.mod module path"}
		res := publishResult(c, "goinstall skipped — module unresolved", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "check the resolved config", Do: "wharfy config"}}
		return res
	}
	// go install と module proxy は v 付きの実タグを使う(homebrew の version 文字列とは別)。
	tag := gitCurrentTag(root)
	gi := &channel.GoInstall{Module: mod, InstallPath: joinModuleMain(mod, cfg.Main), Version: tag, Proxy: goinstallProxy}
	item, _ := gi.Plan(ctx)

	if tagMissing {
		res := publishResult(c, "goinstall: needs a published tag before `go install` resolves a version", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{
			{Reason: "tag so a version exists", Do: "git tag vX.Y.Z && git push --tags"},
			{Reason: "then users install with", Do: gi.InstallCommand()},
		}
		return res
	}

	rs, perr := gi.Probe(ctx)
	if perr != nil {
		res := publishResult(c, "goinstall: cannot reach the module proxy", false, []channel.PlanItem{item})
		res.Errors = []output.Problem{{Code: output.ErrProbeFailed, Message: perr.Error(), Hint: "retry once reachable"}}
		res.Next = []output.NextDo{{Reason: "retry", Do: "wharfy publish goinstall"}}
		return res
	}
	if rs.Found {
		res := publishResult(c, "goinstall: `go install` works at "+tag, true, []channel.PlanItem{item})
		res.Next = []output.NextDo{
			{Reason: "users install with", Do: gi.InstallCommand()},
			{Reason: "review overall state", Do: "wharfy status"},
		}
		return res
	}
	// proxy にまだ無い: エラーではない(伝播待ち/未 push の可能性)。正準コードに合う warning が
	// 無いので誤コードは付けず message/next で案内する。
	res := publishResult(c, "goinstall: "+tag+" not yet on the module proxy (ensure the repo is public and the tag is pushed)", true, []channel.PlanItem{item})
	res.Next = []output.NextDo{
		{Reason: "ensure the tag is pushed (public repo)", Do: "git push --tags"},
		{Reason: "users will install with", Do: gi.InstallCommand()},
	}
	return res
}

// joinModuleMain は module path と main(./cmd/x)から go install 対象パスを作る。
func joinModuleMain(mod, main string) string {
	rel := strings.TrimPrefix(main, "./")
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return mod
	}
	return mod + "/" + rel
}

// channelTargetByName は cfg から指定チャネルの解決済み target を返す。
func channelTargetByName(cfg config.Config, name string) string {
	for _, ch := range cfg.Channels {
		if ch.Name == name {
			return ch.Target
		}
	}
	return ""
}

// channelPushTargetByName は apt/rpm のアップロード先(push)URL を返す。配信と push が別ホストな
// fury.io 等のため Target とは別に持つ。未設定(分離なし)なら Target にフォールバックする。
func channelPushTargetByName(cfg config.Config, name string) string {
	for _, ch := range cfg.Channels {
		if ch.Name == name {
			if ch.PushTarget != "" {
				return ch.PushTarget
			}
			return ch.Target
		}
	}
	return ""
}

// homebrewDeps / scoopDeps は owned チャネルのランタイム依存を返す。横断 runtime_deps と
// per-channel 宣言(homebrew.dependencies 等)をマージした結果(契約外の生成入力なので解決後
// Config ではなく File から射影する)。射影規則は config パッケージに集約。
func homebrewDeps(in config.File) []string { return config.HomebrewDeps(in) }

func scoopDeps(in config.File) []string { return config.ScoopDeps(in) }

// homebrewPublisher / scoopPublisher は archive から各 Publisher を組む。
func homebrewPublisher(cfg config.Config, in config.File, tap, tapOwner, tapRepo, ghOwner, ghRepo, version string, archs []build.Artifact) *channel.Homebrew {
	return &channel.Homebrew{
		Project: cfg.Project,
		Tap:     tap,
		Store:   newTapStore(tapOwner, tapRepo, os.Getenv("GITHUB_TOKEN")),
		Input: channel.FormulaInput{
			Project:      cfg.Project,
			Description:  in.Description,
			Homepage:     cfg.Homepage,
			License:      cfg.License,
			Version:      version,
			Dependencies: homebrewDeps(in),
			Notice:       channelNotice(cfg, "homebrew"),
			Archives:     formulaArchives(archs, ghOwner, ghRepo, cfg.Project, version),
		},
	}
}

func scoopPublisher(cfg config.Config, in config.File, bucket, bOwner, bRepo, ghOwner, ghRepo, version string, archs []build.Artifact) *channel.Scoop {
	input := channel.ScoopInput{
		Project:      cfg.Project,
		Description:  in.Description,
		Homepage:     cfg.Homepage,
		License:      cfg.License,
		Version:      version,
		Dependencies: scoopDeps(in),
		Owner:        ghOwner,
		Repo:         ghRepo,
		Notice:       channelNotice(cfg, "scoop"),
		Archives:     scoopArchives(archs, ghOwner, ghRepo, cfg.Project, version),
	}
	if cfg.Bundle {
		// GUI: 持ち込み windows zip(ポータブル)を参照する app manifest(<project>-app)にする(依頼③)。
		input.App = true
		input.AppName = caskDisplayName(cfg, in)          // 表示名は cask と共有
		input.ExeName = caskDisplayName(cfg, in) + ".exe" // zip 内の <App>.exe
		input.Archives = scoopBundleArchives(archs, ghOwner, ghRepo, version)
	}
	return &channel.Scoop{
		Project: cfg.Project,
		Token:   scoopToken(cfg, in),
		Bucket:  bucket,
		Store:   newTapStore(bOwner, bRepo, os.Getenv("GITHUB_TOKEN")),
		Input:   input,
	}
}

// scoopToken は scoop manifest の名前。GUI(bundle)は <project>-app、CLI は <project>(依頼③)。
func scoopToken(cfg config.Config, in config.File) string {
	if cfg.Bundle {
		return caskToken(cfg, in) // cask と同じ <project>-app 規約を共有
	}
	return cfg.Project
}

// scoopBundleArchives は BYO-bundle の windows zip(ポータブル)を ScoopArch にする(URL は
// 持ち込みファイル名そのまま・依頼③)。
func scoopBundleArchives(archs []build.Artifact, ghOwner, ghRepo, version string) []channel.ScoopArch {
	var out []channel.ScoopArch
	for _, a := range archs {
		if a.OS != "windows" || a.Kind != "zip" {
			continue
		}
		name := filepath.Base(a.Path)
		url := fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", ghOwner, ghRepo, version, name)
		out = append(out, channel.ScoopArch{Arch: a.Arch, URL: url, SHA256: a.SHA256})
	}
	return out
}

// caskToken は cask の識別子/ファイル名。明示 token 優先、既定は <project>-app(依頼書 §4 の命名規約:
// CLI=<project> / GUI=<project>-app と別ラベルにする)。
func caskToken(cfg config.Config, in config.File) string {
	if in.Cask != nil && in.Cask.Token != "" {
		return in.Cask.Token
	}
	return cfg.Project + "-app"
}

// caskDisplayName は cask の表示名 "<App>"(name / app stanza)。cask.name > bundle.name > project。
func caskDisplayName(cfg config.Config, in config.File) string {
	if in.Cask != nil && in.Cask.Name != "" {
		return in.Cask.Name
	}
	if in.Bundle != nil && in.Bundle.Name != "" {
		return in.Bundle.Name
	}
	return cfg.Project
}

// caskAppBundle は app stanza の対象 "<App>.app"。明示 app 優先、既定は "<表示名>.app"。
func caskAppBundle(cfg config.Config, in config.File) string {
	if in.Cask != nil && in.Cask.App != "" {
		return in.Cask.App
	}
	return caskDisplayName(cfg, in) + ".app"
}

// caskArtifacts は darwin の持ち込みバンドル(dmg/zip)を cask の url+sha256 にする。アセット名は
// bundleRelease と同じく持ち込みファイル名をそのまま使う(url と実アセットの齟齬を防ぐ)。
func caskArtifacts(archs []build.Artifact, ghOwner, ghRepo, version string) []channel.CaskArtifact {
	var out []channel.CaskArtifact
	for _, a := range archs {
		if a.OS != "darwin" {
			continue
		}
		if a.Kind != "dmg" && a.Kind != "zip" {
			continue
		}
		name := filepath.Base(a.Path)
		url := fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", ghOwner, ghRepo, version, name)
		out = append(out, channel.CaskArtifact{Arch: a.Arch, URL: url, SHA256: a.SHA256})
	}
	channel.SortCaskArtifacts(out)
	return out
}

// caskNotarizeWarning は非 notarized バンドルを Cask で配る際の Gatekeeper 挙動を明示する(依頼⑤)。
// wharfy は再署名も notarize もしない(relay)ので、初回起動で警告が出ることを利用者/エージェントに
// 先出しする。回避手順は cask の caveats にも書いてある。machine-readable なコードで agent が拾える。
func caskNotarizeWarning(cfg config.Config, in config.File) output.Warning {
	name := caskDisplayName(cfg, in)
	return output.Warning{
		Code:    output.WarnDarwinUnnotarized,
		Message: name + " ships without notarization — macOS Gatekeeper warns on first launch; the cask caveats tell users to right-click → Open (wharfy does not notarize)",
	}
}

// caskPublisher は bundle 成果物から Cask Publisher を組む(homebrewPublisher の対)。
func caskPublisher(cfg config.Config, in config.File, tap, tapOwner, tapRepo, ghOwner, ghRepo, version string, archs []build.Artifact) *channel.Cask {
	return &channel.Cask{
		Token: caskToken(cfg, in),
		Tap:   tap,
		Store: newTapStore(tapOwner, tapRepo, os.Getenv("GITHUB_TOKEN")),
		Input: channel.CaskInput{
			Token:     caskToken(cfg, in),
			Name:      caskDisplayName(cfg, in),
			Desc:      in.Description,
			Homepage:  cfg.Homepage,
			Version:   version,
			AppBundle: caskAppBundle(cfg, in),
			Notarized: false, // 依頼元は notarize しない方針(依頼⑤)。caveats で Gatekeeper を案内する
			Notice:    channelNotice(cfg, "cask"),
			Artifacts: caskArtifacts(archs, ghOwner, ghRepo, version),
		},
	}
}

// publishCask は cask チャネル(owned)。持ち込みバンドルを GitHub Release へ上げ、実 sha256 で
// 同一 tap の Casks/<token>.rb を書く(Formula と同居=状態一元化・依頼④)。publishHomebrew の対だが、
// 成果物は archive でなく BYO-bundle(bundleRelease)から得る。
func publishCask(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool, fz *channelFreeze) output.Result {
	tap := channelTargetByName(cfg, "cask")
	tapOwner, tapRepo, tapOK := splitOwnerName(tap)
	ghOwner, ghRepo, ghOK := splitOwnerName(cfg.Github)
	if tap == "" || !tapOK || !ghOK {
		return ownedSkip(c, "cask", "cask tap/github unresolved — set 'github' or 'cask.tap' in wharfy.yaml")
	}
	if !config.IsBundle(in) {
		return ownedSkip(c, "cask", "cask needs a BYO-bundle input — declare 'bundle:' in wharfy.yaml")
	}

	if !flagYes {
		// preview: バンドルを検証して実 sha を得る(アップロードしない)ため plan の差分を出す。
		// 凍結中は手元のバンドルではなく、その版を配ったときの成果物が真実。
		archs, aerr := frozenArtifacts(fz, func() ([]build.Artifact, error) { return build.ValidateBundles(root, toBundles(in)) })
		if aerr != nil {
			return buildErrorResult(c, aerr)
		}
		pub := caskPublisher(cfg, in, tap, tapOwner, tapRepo, ghOwner, ghRepo, version, archs)
		res := ownedReleaseDryRun(ctx, c, pub, version, "cask", tap, tagMissing)
		res.Warnings = append(res.Warnings, caskNotarizeWarning(cfg, in)) // 依頼⑤: 非 notarized を先出し
		return res
	}

	if tagMissing {
		return tagMissingResult(c, version)
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return tokenMissingResult(c)
	}
	// 実 release: バンドルを GitHub Release へ上げ、実 sha256 を得る(記録済みなら再利用)。
	// 凍結中は release を走らせない — 配り直すのは既に release 済みの版だから。
	archs, aerr := frozenArtifacts(fz, func() ([]build.Artifact, error) {
		if set, found, _ := build.LoadArtifacts(root); found && set.Version == version {
			return set.Artifacts, nil
		}
		a, rerr := bundleRelease(ctx, root, cfg, version, in)
		if rerr != nil {
			return nil, rerr
		}
		_ = build.SaveArtifacts(root, version, a)
		return a, nil
	})
	if aerr != nil {
		return buildErrorResult(c, aerr)
	}
	pub := caskPublisher(cfg, in, tap, tapOwner, tapRepo, ghOwner, ghRepo, version, archs)
	// 書き込み前の自己検査(#10): cask の url が指すバンドルと記録 sha が食い違えば止める。
	if err := verifyManifestChecksums(pub, archs); err != nil {
		return checksumMismatchResult(c, "cask", err)
	}
	res := ownedReleaseApply(ctx, c, pub, root, cfg.Project, "cask", tap, cfg.Github, version, archs, fz)
	res.Warnings = append(res.Warnings, caskNotarizeWarning(cfg, in)) // 依頼⑤: 非 notarized を先出し
	return res
}

// ownedReleaseDryRun は plan をプレビューする(書かない)。requires で実 apply の前提条件を先出し。
func ownedReleaseDryRun(ctx context.Context, c registry.Command, pub channel.Publisher, version, chName, target string, tagMissing bool) output.Result {
	item, err := pub.Plan(ctx)
	if err != nil {
		return probeFailedResult(c, chName, err)
	}
	// 配線不能(空 architecture など)は skip。壊れたマニフェストを予告せず、理由を surface する(依頼書七通目 §1)。
	if item.Action == channel.ActionSkip {
		return ownedSkipItem(c, chName, item)
	}
	reqs := applyRequirements(tagMissing)
	msg := "plan: " + item.Action + " " + item.OwnedArtifact
	msg += previewNote(version, tagMissing, true)
	res := output.New(c.Name, msg, true)
	res.Data = publishData{Applied: false, Plan: []channel.PlanItem{item}, Requires: reqs}
	res.Next = dryRunNext(item, reqs, chName)
	// 自前リポジトリ(tap/bucket)が未作成なら予告する(--yes で wharfy が作る)。
	if rb, ok := pub.(channel.RepoBacked); ok {
		if exists, e := rb.RepoExists(ctx); e == nil && !exists {
			res.Warnings = append(res.Warnings, output.Warning{
				Code:    output.WarnTapWillBeCreated,
				Message: target + " does not exist yet — wharfy will create it on --yes",
			})
		}
	}
	return res
}

// previewNote は dry-run の message 注記。sha256 を含むプレビュー(formula/manifest/PKGBUILD/
// winget installer)は snapshot ビルド由来の暫定値なので、「実値は --yes で確定」と正直に明示する
// (follow-up #4)。tag が無い時はその旨も併記する。正準コードに合う warning が無いので message で示す。
func previewNote(version string, tagMissing, hasChecksums bool) string {
	var parts []string
	if hasChecksums {
		parts = append(parts, "checksums are provisional (snapshot); real values are set on --yes")
	}
	if tagMissing {
		parts = append(parts, "no git tag yet, previewing @ "+version)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (preview: " + strings.Join(parts, "; ") + ")"
}

// applyRequirements は --yes の前提条件と現在の充足状況を返す(preview で先出しする)。
func applyRequirements(tagMissing bool) []requirement {
	return []requirement{
		{Requirement: "git tag", Met: !tagMissing, Hint: "git tag vX.Y.Z && git push --tags (the tag is the version)"},
		{Requirement: "GITHUB_TOKEN", Met: os.Getenv("GITHUB_TOKEN") != "", Hint: "export GITHUB_TOKEN=… (write access to the tap)"},
	}
}

// dryRunNext は noop なら verify、差分ありなら未充足の前提を先に解消してから --yes を促す。
func dryRunNext(item channel.PlanItem, reqs []requirement, chName string) []output.NextDo {
	if item.Action == channel.ActionNoop {
		return []output.NextDo{{Reason: "already up to date; verify install", Do: "wharfy verify"}}
	}
	next := []output.NextDo{}
	var unmetCred bool
	for _, r := range reqs {
		if r.Met {
			continue
		}
		next = append(next, output.NextDo{Reason: "required before --yes: " + r.Requirement, Do: r.Hint})
		if _, ok := registry.Credentials[r.Requirement]; ok {
			unmetCred = true
		}
	}
	// 資格情報が欠けているなら、手元の export だけでなく CI への登録の仕方も要る(D-12)。
	if unmetCred {
		next = append(next, output.NextDo{Reason: "what to register, and how, for CI", Do: "wharfy secrets"})
	}
	next = append(next, output.NextDo{Reason: "apply the shown changes", Do: "wharfy publish " + chName + " --yes"})
	return next
}

// writeGeneratedConfig は所有する生成物(goreleaser.yaml ＋ script 有効時は install.sh)を
// .wharfy/ に書く。install.sh は extra_files が参照するので、生成設定と必ず同時に書く。
//
// BYO-binary(依頼①)では GoReleaser 設定を生成しない(非 Go リポでは main が無く生成できない)。
// install.sh だけ書き、configPath は空("")を返す — 後段の archive/release は prebuilt seam が
// GoReleaser を通さず処理する。
func writeGeneratedConfig(root string, cfg config.Config, in config.File, version string) (string, error) {
	scriptVer, ship := installScriptTarget(root, cfg, version)
	if cfg.Prebuilt {
		if config.HasChannel(cfg, "script") && ship {
			if err := writeInstallScripts(root, cfg, scriptVer); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	glYAML, err := config.GenerateGoReleaser(cfg, in)
	if err != nil {
		return "", err
	}
	if config.HasChannel(cfg, "script") && ship {
		if err := writeInstallScripts(root, cfg, scriptVer); err != nil {
			return "", err
		}
	}
	if config.HasChannel(cfg, "container") {
		// dockers の dockerfile が参照するので、生成設定と必ず同時に書く。
		if _, err := config.WriteDockerfile(root, config.GenerateDockerfile(cfg)); err != nil {
			return "", err
		}
	}
	return config.WriteGoReleaser(root, glYAML)
}

// tagMissingResult / tokenMissingResult は実 apply の前提不足。実リリース前に弾く。
func tagMissingResult(c registry.Command, version string) output.Result {
	res := output.New(c.Name, "cannot publish without a tag", false)
	res.Errors = []output.Problem{{Code: output.ErrTagMissing, Message: "no git tag found; the tag is the version", Hint: "git tag vX.Y.Z && git push --tags, then retry"}}
	res.Next = []output.NextDo{{Reason: "tag the release", Do: "git tag v" + version + " && git push --tags"}}
	return res
}

func tokenMissingResult(c registry.Command) output.Result {
	res := output.New(c.Name, "cannot publish without a token", false)
	res.Errors = []output.Problem{{Code: output.ErrTokenMissing, Message: "GITHUB_TOKEN required to upload the release and write the tap", Hint: "export GITHUB_TOKEN=…"}}
	res.Next = []output.NextDo{{Reason: "set the token then retry", Do: "export GITHUB_TOKEN=… ; wharfy publish homebrew --yes"}}
	return res
}

// ownedReleaseApply は実 archive 反映後に formula/manifest を所有リポジトリに書く(--yes)。
// 前提(tag/token)は確認済み。archive は既に GitHub Releases へアップロード済み(実 checksum)。
func ownedReleaseApply(ctx context.Context, c registry.Command, pub channel.Publisher, root, project, chName, target, releaseTarget, version string, archs []build.Artifact, fz *channelFreeze) output.Result {
	// 自前リポジトリ(tap/bucket)が無ければ作る(--yes の明示同意があるので)。
	created := false
	if rb, ok := pub.(channel.RepoBacked); ok {
		c2, err := rb.EnsureRepo(ctx)
		if err != nil {
			res := output.New(c.Name, "failed to create "+target, false)
			res.Errors = []output.Problem{{Code: output.ErrTargetCreateFailed, Message: err.Error(), Hint: "check token scope (repo create) or create " + target + " manually"}}
			res.Next = []output.NextDo{{Reason: "fix permissions then retry", Do: "wharfy publish " + chName + " --yes"}}
			return res
		}
		created = c2
	}

	item, pubres, err := pub.Publish(ctx)
	if err != nil {
		res := output.New(c.Name, "publish failed", false)
		res.Errors = []output.Problem{{Code: output.ErrPublishFailed, Message: err.Error(), Hint: "check token scope and repo permissions"}}
		res.Next = []output.NextDo{{Reason: "fix the cause then retry", Do: "wharfy publish " + chName + " --yes"}}
		return res
	}
	// 配線不能なら Publish は書いていない。published と偽らず・state に記録せず skip を surface する(依頼書七通目 §1)。
	if item.Action == channel.ActionSkip {
		return ownedSkipItem(c, chName, item)
	}

	if st, err := state.Load(root, project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		now := nowUTC().Format(time.RFC3339)
		// releases(archive アップロード)とチャネル(formula/manifest)の両方を記録する。
		// 凍結中は release を走らせていないので、releases の記録は新版のまま触らない。
		if fz == nil {
			st.Publish["releases"] = state.PublishRecord{Version: version, Target: releaseTarget, At: now}
		}
		rec := state.PublishRecord{Version: version, Target: target, Commit: pubres.Commit, At: now}
		if freezeKeepsArtifacts(chName) {
			rec.Artifacts = archs // 畳んだあと、この版で作り直すための拠り所(D-3)
		}
		st.Publish[chName] = rec
		_ = state.Save(root, st)
	}

	item.Action = channel.ActionUpdate // 反映済みの操作を明示(create/update いずれも書いた)
	res := publishResult(c, "published "+project+" "+version+" → "+target, true, []channel.PlanItem{item})
	res.Data = publishData{Applied: true, Plan: []channel.PlanItem{item}}
	if created {
		res.Warnings = append(res.Warnings, output.Warning{Code: output.WarnTapWillBeCreated, Message: "created " + target})
	}
	res.Next = []output.NextDo{{Reason: "install from the channel and run it", Do: "wharfy verify"}}
	return res
}

// scoopArchives は build の archive(windows)を Releases の zip URL 付き ScoopArch にする。
func scoopArchives(archs []build.Artifact, ghOwner, ghRepo, project, version string) []channel.ScoopArch {
	var out []channel.ScoopArch
	for _, a := range archs {
		if a.OS != "windows" {
			continue
		}
		name := fmt.Sprintf("%s_%s_windows_%s.zip", project, version, a.Arch)
		url := fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", ghOwner, ghRepo, version, name)
		out = append(out, channel.ScoopArch{Arch: a.Arch, URL: url, SHA256: a.SHA256})
	}
	return out
}

// --- ヘルパ ---

func publishResult(c registry.Command, msg string, ok bool, plan []channel.PlanItem) output.Result {
	res := output.New(c.Name, msg, ok)
	res.Data = publishData{Applied: false, Plan: plan}
	res.Next = []output.NextDo{}
	return res
}

// probeFailedResult は plan 前の実体照合に失敗したときの結果。next は照合できなかった当のチャネルを
// 指す(呼び手は owned 全チャネル。homebrew 決め打ちだと、設定に無いチャネルを勧めうる)。
func probeFailedResult(c registry.Command, chName string, err error) output.Result {
	res := output.New(c.Name, "cannot read the "+chName+" target", false)
	res.Errors = []output.Problem{{Code: output.ErrProbeFailed, Message: err.Error(), Hint: "check network or target visibility"}}
	res.Next = []output.NextDo{{Reason: "retry once reachable", Do: "wharfy publish " + chName + " --dry-run"}}
	return res
}

func mainAmbiguousResult(c registry.Command, cfg config.Config, amb *config.AmbiguousMainError) output.Result {
	res := output.New(c.Name, "cannot publish: 'main' is ambiguous", false)
	res.Errors = []output.Problem{{Code: output.ErrMainAmbiguous, Message: amb.Error(), Hint: "set 'main' in wharfy.yaml"}}
	res.Next = []output.NextDo{{Reason: "resolve the build target", Do: "wharfy config"}}
	return res
}

// homebrewTarget は cfg から homebrew の tap(owner/homebrew-project)を返す。
func homebrewTarget(cfg config.Config) (string, bool) {
	for _, ch := range cfg.Channels {
		if ch.Name == "homebrew" {
			return ch.Target, ch.Target != ""
		}
	}
	return "", false
}

// publishVersion は tag(先頭 v 除去)を返す。tag が無ければ "0.0.0" とプレビュー扱い。
func publishVersion(root string) (version string, tagMissing bool) {
	tag := gitCurrentTag(root)
	if tag == "" {
		return "0.0.0", true
	}
	return strings.TrimPrefix(tag, "v"), false
}

// formulaArchives は build の archive(darwin/linux の .tar.gz)を Releases の URL 付き ArchiveRef にする。
// tar.gz 以外(bundle の dmg/appimage、Linux Package の deb/rpm、windows の zip)は formula の対象外。
// 混ぜると URL は CLI の <project>_<ver>_<os>_<arch>.tar.gz を指すのに sha だけ別ファイルのものになり
// (例: linux/amd64 の .rpm が同 os/arch の tarball 参照を汚染する / darwin の dmg が cask と同一 sha を
// 記録する)、brew が checksum 不一致で全 artifact を弾く事故になる。Kind だけでは足りない — GoReleaser の
// Linux Package(deb/rpm)は Kind 空で archive と同じ os/arch を持つため、拡張子 .tar.gz で厳密に絞る。
func formulaArchives(archs []build.Artifact, ghOwner, ghRepo, project, version string) []channel.ArchiveRef {
	var out []channel.ArchiveRef
	for _, a := range archs {
		if a.OS != "darwin" && a.OS != "linux" {
			continue // homebrew は darwin/linux のみ
		}
		if !strings.HasSuffix(a.Path, ".tar.gz") {
			continue // deb/rpm/dmg/appimage 等は formula の archive ではない — cask/linuxrepo が扱う
		}
		name := fmt.Sprintf("%s_%s_%s_%s.tar.gz", project, version, a.OS, a.Arch)
		url := fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", ghOwner, ghRepo, version, name)
		out = append(out, channel.ArchiveRef{OS: a.OS, Arch: a.Arch, URL: url, SHA256: a.SHA256})
	}
	channel.SortArchives(out)
	return out
}

// verifyManifestChecksums は pub(ChecksumSource)が manifest に書く各 (URL→sha256) を、その URL の
// 資産名に対応する実 artifact の sha256 と突き合わせる(#10 の自己検査)。不一致、または URL の指す
// 資産が upload 済み成果物に無い場合は非 nil を返し publish を止める。これで #9 のような『URL は CLI
// tarball を指すのに sha は bundle のもの』という取り違えや、URL/資産名のずれ(404 誘発)を書き込み前に捕まえる。
// sha を書かないチャネル(ChecksumSource 未実装=container/script/releases)は nil(検査対象外)。
func verifyManifestChecksums(pub channel.Publisher, archs []build.Artifact) error {
	src, ok := pub.(channel.ChecksumSource)
	if !ok {
		return nil
	}
	bySHA := make(map[string]string, len(archs))
	for _, a := range archs {
		bySHA[filepath.Base(a.Path)] = a.SHA256
	}
	for _, ref := range src.ManifestRefs() {
		asset := ref.URL[strings.LastIndexByte(ref.URL, '/')+1:] // URL 末尾が upload 資産名
		want, ok := bySHA[asset]
		if !ok {
			return fmt.Errorf("manifest references %q but no uploaded artifact carries that name", asset)
		}
		if !strings.EqualFold(want, ref.SHA256) {
			return fmt.Errorf("sha256 mismatch for %s: manifest records %s but the uploaded artifact is %s", asset, ref.SHA256, want)
		}
	}
	return nil
}

// checksumMismatchResult は自己検査(verifyManifestChecksums)失敗を error として返す(半端な tap push を防ぐ)。
func checksumMismatchResult(c registry.Command, chName string, err error) output.Result {
	res := output.New(c.Name, chName+" checksum self-check failed — refusing to publish a mismatched manifest", false)
	res.Errors = []output.Problem{{
		Code:    output.ErrChecksumMismatch,
		Message: err.Error(),
		Hint:    "the manifest sha256 does not match the uploaded asset; re-run 'wharfy release --yes' to rebuild artifacts, then retry publish",
	}}
	res.Next = []output.NextDo{{Reason: "rebuild artifacts then retry", Do: "wharfy release --yes"}}
	return res
}

func splitOwnerName(s string) (owner, name string, ok bool) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
