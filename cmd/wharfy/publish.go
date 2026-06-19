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

// 差し替え点(テストで fake 化＝末端は差し替え可能・01)。
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

// runPublish は所有チャネルへ発行する。書く前に必ず差分(plan)を見せる(設計 02/03)。
// --yes 無し: plan のプレビュー(applied:false)。--yes: 実書き込み(applied:true)。
// 実装済み: homebrew / goinstall。未対応チャネルは plan で skip を返す(型は同一)。
func runPublish(ctx context.Context, c registry.Command, args []string) output.Result {
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

	// 引数なし = 全チャネル一括(release は 1 回・多重 release 衝突を避ける)。
	if len(args) == 0 {
		return publishAll(ctx, c, root, cfg, in, version, tagMissing)
	}

	switch args[0] {
	case "homebrew":
		return publishHomebrew(ctx, c, root, cfg, in, version, tagMissing)
	case "scoop":
		return publishScoop(ctx, c, root, cfg, in, version, tagMissing)
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
		return publishAur(ctx, c, root, cfg, in, version, tagMissing)
	case "goinstall":
		return publishGoinstall(ctx, c, root, cfg, tagMissing)
	case "script":
		return publishScript(ctx, c, root, cfg, in, version, tagMissing)
	default:
		item := channel.PlanItem{
			Channel: args[0], Action: channel.ActionSkip,
			Reason: "unknown channel (owned: homebrew/scoop/apt/rpm/container/aur/script/goinstall, gated: winget/homebrew-core)",
		}
		res := publishResult(c, "channel "+args[0]+" not implemented", false, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "publish a supported channel or all", Do: "wharfy publish"}}
		return res
	}
}

// implementedChannels は cfg.Channels のうち publish が扱える順序付きリスト。
func implementedChannels(cfg config.Config) []string {
	known := map[string]bool{
		"homebrew": true, "scoop": true, "apt": true, "rpm": true, "container": true,
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
		reqs = append(reqs, requirement{Requirement: "PACKAGE_REPO_TOKEN", Met: os.Getenv("PACKAGE_REPO_TOKEN") != "", Hint: "export PACKAGE_REPO_TOKEN=… (apt/rpm hosted repo)"})
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
		var items []channel.PlanItem
		for _, ch := range chans {
			items = append(items, planChannelSummary(ch, cfg))
		}
		res := output.New(c.Name, fmt.Sprintf("plan: %d channel(s)", len(items)), true)
		res.Data = publishData{Applied: false, Plan: items, Requires: reqs}
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
	skipDocker := !(containsStr(chans, "container") && dockerOK)

	configPath, err := writeGeneratedConfig(root, cfg, in, version)
	if err != nil {
		return internalError(c, err)
	}
	// release が済んでいれば(同 version)再アップロードしない(c2)。途中失敗からの再開で高コストな
	// release を繰り返さない土台。無ければ 1 回だけ走らせて記録する。
	var archs []build.Artifact
	if set, found, _ := build.LoadArtifacts(root); found && set.Version == version {
		archs = set.Artifacts
	} else {
		a, rerr := newMultiReleaser(config.DistDir).ReleaseAll(ctx, root, configPath, skipDocker)
		if rerr != nil {
			return buildErrorResult(c, rerr)
		}
		_ = build.SaveArtifacts(root, version, a)
		archs = a
	}

	st, _ := state.Load(root, cfg.Project)
	if st.Publish == nil {
		st.Publish = map[string]state.PublishRecord{}
	}
	now := nowUTC().Format(time.RFC3339)

	var items []channel.PlanItem
	var warns []output.Warning
	for _, ch := range chans {
		// state 認識の再開(b): その version で発行済みのチャネルは飛ばす。途中失敗後の再実行で
		// 完了済みを再処理しない(残った失敗チャネルだけ進む)。
		if rec, ok := st.Publish[ch]; ok && rec.Version == version {
			items = append(items, channel.PlanItem{Channel: ch, Kind: config.Kind(ch), Action: channel.ActionNoop, Reason: "already published at " + version})
			continue
		}
		item, w, aerr := applyChannel(ctx, ch, cfg, in, version, ghOwner, ghRepo, archs, st, now, dockerOK)
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
	res.Warnings = warns
	res.Next = []output.NextDo{{Reason: "verify installs work", Do: "wharfy verify"}}
	return res
}

// planChannelSummary は一括 preview 用の軽量 plan(発行先＋操作。差分は出さない)。
func planChannelSummary(ch string, cfg config.Config) channel.PlanItem {
	target := channelTargetByName(cfg, ch)
	it := channel.PlanItem{Channel: ch, Kind: config.Kind(ch), Action: channel.ActionCreate}
	switch ch {
	case "homebrew":
		it.OwnedArtifact = orUnresolved(target, "Formula/"+cfg.Project+".rb")
	case "scoop":
		it.OwnedArtifact = orUnresolved(target, "bucket/"+cfg.Project+".json")
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
func applyChannel(ctx context.Context, ch string, cfg config.Config, in config.File, version, ghOwner, ghRepo string, archs []build.Artifact, st *state.State, now string, dockerOK bool) (channel.PlanItem, *output.Warning, error) {
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
		if _, err := hb.EnsureRepo(ctx); err != nil { // 未作成なら tap を作る(ADR-8)
			return channel.PlanItem{}, nil, err
		}
		item, pub, err := hb.Publish(ctx)
		if err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish["homebrew"] = state.PublishRecord{Version: version, Target: tap, Commit: pub.Commit, At: now}
		item.Action = channel.ActionUpdate
		return item, nil, nil

	case "scoop":
		bucket := channelTargetByName(cfg, "scoop")
		bo, br, ok := splitOwnerName(bucket)
		if !ok {
			return skip("bucket unresolved")
		}
		sc := scoopPublisher(cfg, in, bucket, bo, br, ghOwner, ghRepo, version, archs)
		if _, err := sc.EnsureRepo(ctx); err != nil { // 未作成なら bucket を作る(ADR-8)
			return channel.PlanItem{}, nil, err
		}
		item, pub, err := sc.Publish(ctx)
		if err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish["scoop"] = state.PublishRecord{Version: version, Target: bucket, Commit: pub.Commit, At: now}
		item.Action = channel.ActionUpdate
		return item, nil, nil

	case "apt", "rpm":
		repo := channelTargetByName(cfg, ch)
		if repo == "" {
			return skip(ch + ".repo not set")
		}
		token := os.Getenv("PACKAGE_REPO_TOKEN")
		if token == "" {
			return skip("PACKAGE_REPO_TOKEN not set")
		}
		ext := map[string]string{"apt": ".deb", "rpm": ".rpm"}[ch]
		if _, err := uploadLinuxPackages(ctx, archs, ext, repo, token); err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish[ch] = state.PublishRecord{Version: version, Target: repo, At: now}
		return mk(channel.KindOwned, channel.ActionUpdate, repo), nil, nil

	case "container":
		image := channelTargetByName(cfg, "container")
		if !dockerOK {
			return skip("docker unavailable")
		}
		st.Publish["container"] = state.PublishRecord{Version: version, Target: image, At: now}
		return mk(channel.KindOwned, channel.ActionUpdate, image), nil, nil // ReleaseAll が push 済み

	case "script":
		st.Publish["script"] = state.PublishRecord{Version: version, Target: cfg.Github + " release:" + config.InstallScriptName, At: now}
		return mk(channel.KindOwned, channel.ActionUpdate, cfg.Github+" release:"+config.InstallScriptName), nil, nil // ReleaseAll が install.sh を同梱済み

	case "aur":
		pkg := channelTargetByName(cfg, "aur")
		sshKey := os.Getenv("AUR_SSH_KEY")
		if sshKey == "" {
			return skip("AUR_SSH_KEY not set")
		}
		ai := channel.AurInput{Package: pkg, Project: cfg.Project, Version: version, License: cfg.License,
			Description: in.Description, Homepage: cfg.Homepage, Maintainer: aurMaintainer(ghOwner),
			Sources: aurSources(archs, ghOwner, ghRepo, cfg.Project, version)}
		commit, err := newAurPusher(sshKey).Push(ctx, pkg, ai.Files())
		if err != nil {
			return channel.PlanItem{}, nil, err
		}
		st.Publish["aur"] = state.PublishRecord{Version: version, Target: pkg, Commit: commit, At: now}
		return mk(channel.KindOwned, channel.ActionUpdate, "aur:"+pkg), nil, nil

	case "winget":
		identifier := channelTargetByName(cfg, "winget")
		wi := channel.WingetInput{Identifier: identifier, Project: cfg.Project, Version: version, License: cfg.License,
			Description: in.Description, Homepage: cfg.Homepage, Installers: wingetInstallers(archs, ghOwner, ghRepo, cfg.Project, version)}
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
			Project: cfg.Project, Binary: cfg.Project, Description: in.Description, Homepage: cfg.Homepage,
			License: cfg.License, Version: version, SourceURL: sourceTarballURL(ghOwner, ghRepo, version), SourceSHA: sha})
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
func publishHomebrew(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool) output.Result {
	tap, ok := homebrewTarget(cfg)
	tapOwner, tapRepo, tapOK := splitOwnerName(tap)
	ghOwner, ghRepo, ghOK := splitOwnerName(cfg.Github)
	if !ok || !tapOK || !ghOK {
		return ownedSkip(c, "homebrew", "homebrew tap/github unresolved — set 'github' or 'homebrew.tap' in wharfy.yaml")
	}
	return publishViaRelease(ctx, c, root, cfg, in, version, tagMissing, "homebrew", tap,
		func(archs []build.Artifact) channel.Publisher {
			return homebrewPublisher(cfg, in, tap, tapOwner, tapRepo, ghOwner, ghRepo, version, archs)
		})
}

func publishScoop(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool) output.Result {
	bucket := channelTargetByName(cfg, "scoop")
	bOwner, bRepo, bOK := splitOwnerName(bucket)
	ghOwner, ghRepo, ghOK := splitOwnerName(cfg.Github)
	if bucket == "" || !bOK || !ghOK {
		return ownedSkip(c, "scoop", "scoop bucket/github unresolved — set 'github' or 'scoop.bucket' in wharfy.yaml")
	}
	return publishViaRelease(ctx, c, root, cfg, in, version, tagMissing, "scoop", bucket,
		func(archs []build.Artifact) channel.Publisher {
			return scoopPublisher(cfg, in, bucket, bOwner, bRepo, ghOwner, ghRepo, version, archs)
		})
}

// publishAur は aur チャネル(owned・03)。-bin パッケージの PKGBUILD/.SRCINFO を生成し、
// AUR の自前 git(ssh)へ push する(審査なし)。linux tarball の実 sha を参照する。
func publishAur(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool) output.Result {
	pkg := channelTargetByName(cfg, "aur")
	ghOwner, ghRepo, ghOK := splitOwnerName(cfg.Github)
	if pkg == "" || !ghOK {
		item := channel.PlanItem{Channel: "aur", Kind: channel.KindOwned, Action: channel.ActionSkip,
			Reason: "aur package/github unresolved — set 'github' or 'aur.package'"}
		res := publishResult(c, "aur skipped — unresolved", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "check the resolved config", Do: "wharfy config"}}
		return res
	}

	configPath, err := writeGeneratedConfig(root, cfg, in, version)
	if err != nil {
		return internalError(c, err)
	}
	buildInput := func(archs []build.Artifact) channel.AurInput {
		return channel.AurInput{
			Package:     pkg,
			Project:     cfg.Project,
			Version:     version,
			License:     cfg.License,
			Description: in.Description,
			Homepage:    cfg.Homepage,
			Maintainer:  aurMaintainer(ghOwner),
			Sources:     aurSources(archs, ghOwner, ghRepo, cfg.Project, version),
		}
	}
	reqs := aurRequirements(tagMissing)

	if !flagYes {
		archs, aerr := newArchiver(config.DistDir).Archives(ctx, root, configPath)
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
	archs, rerr := newReleaser(config.DistDir).Release(ctx, root, configPath)
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
		st.Publish["releases"] = state.PublishRecord{Version: version, Target: cfg.Github, At: now}
		st.Publish["aur"] = state.PublishRecord{Version: version, Target: pkg, Commit: commit, At: now}
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

// publishWinget は winget チャネル(gated・11A)。manifest 3 種を生成し、microsoft/winget-pkgs を
// fork→branch→commit→PR まで組み立てる(マージはしない)。書く前に申請物を見せる。
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

	configPath, err := writeGeneratedConfig(root, cfg, in, version)
	if err != nil {
		return internalError(c, err)
	}
	buildInput := func(archs []build.Artifact) channel.WingetInput {
		return channel.WingetInput{
			Identifier:  identifier,
			Project:     cfg.Project,
			Version:     version,
			License:     cfg.License,
			Description: in.Description,
			Homepage:    cfg.Homepage,
			Installers:  wingetInstallers(archs, ghOwner, ghRepo, cfg.Project, version),
		}
	}
	reqs := applyRequirements(tagMissing)

	if !flagYes {
		archs, aerr := newArchiver(config.DistDir).Archives(ctx, root, configPath)
		if aerr != nil {
			return buildErrorResult(c, aerr)
		}
		files := channel.GenerateWingetManifests(buildInput(archs))
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
	// 実 release: windows zip を GitHub Releases へ上げ、実 sha256 を得る(installer が参照)。
	archs, rerr := newReleaser(config.DistDir).Release(ctx, root, configPath)
	if rerr != nil {
		return buildErrorResult(c, rerr)
	}
	wi := buildInput(archs)
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

// publishHomebrewCore は homebrew-core チャネル(strict gated・*-core・11A)。core は notability ＋
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
			Project: cfg.Project, Binary: cfg.Project, Description: in.Description,
			Homepage: cfg.Homepage, License: cfg.License, Version: version,
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

// publishContainer は container チャネル(ghcr OCI・マルチアーキ・11B)。goreleaser の
// docker pipe で per-arch イメージをビルドし ghcr へ push、manifest list を作る。
// docker デーモン＋ghcr 認証(GITHUB_TOKEN packages:write)が要る。書く前に計画を見せる。
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

	configPath, err := writeGeneratedConfig(root, cfg, in, version)
	if err != nil {
		return internalError(c, err)
	}
	if _, cerr := newContainerizer(config.DistDir).Containers(ctx, root, configPath); cerr != nil {
		return buildErrorResult(c, cerr)
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
// multipart POST でアップロードする(PACKAGE_REPO_TOKEN。GitHub には触れない・03/07)。
// repo 未設定は skip して案内(channel_skipped)。プロバイダ依存のため `-F package=@` 形を既定。
func publishLinuxPkg(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool, chName, ext string) output.Result {
	repo := channelTargetByName(cfg, chName)
	if repo == "" {
		item := channel.PlanItem{Channel: chName, Kind: channel.KindOwned, Action: channel.ActionSkip,
			Reason: chName + ".repo not set — hosted repo URL is required"}
		res := publishResult(c, chName+" skipped — no hosted repo configured", true, []channel.PlanItem{item})
		res.Warnings = []output.Warning{{Code: output.WarnChannelSkipped, Message: chName + " skipped — set " + chName + ".repo and PACKAGE_REPO_TOKEN"}}
		res.Next = []output.NextDo{{Reason: "configure the hosted repo", Do: "set " + chName + ".repo in wharfy.yaml ; export PACKAGE_REPO_TOKEN=…"}}
		return res
	}

	names := expectedPackages(cfg, version, ext)
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
	token := os.Getenv("PACKAGE_REPO_TOKEN")
	if token == "" {
		res := output.New(c.Name, "cannot publish without a token", false)
		res.Errors = []output.Problem{{Code: output.ErrTokenMissing, Message: "PACKAGE_REPO_TOKEN required to upload to the hosted repo", Hint: "export PACKAGE_REPO_TOKEN=…"}}
		res.Next = []output.NextDo{{Reason: "set the token then retry", Do: "export PACKAGE_REPO_TOKEN=… ; wharfy publish " + chName + " --yes"}}
		return res
	}

	configPath, err := writeGeneratedConfig(root, cfg, in, version)
	if err != nil {
		return internalError(c, err)
	}
	pkgs, perr := newPackager(config.DistDir).Packages(ctx, root, configPath)
	if perr != nil {
		return buildErrorResult(c, perr)
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
		if uerr := uploadPackage(ctx, repo, token, full); uerr != nil {
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
	return res
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
		{Requirement: "PACKAGE_REPO_TOKEN", Met: os.Getenv("PACKAGE_REPO_TOKEN") != "", Hint: "export PACKAGE_REPO_TOKEN=… (hosted repo upload)"},
	}
}

// httpUploadPackage は hosted repo へ multipart POST する(field "package"=ファイル、
// 認証は basic auth で username=token。build.example.yaml の curl -F package=@ -u token: 準拠)。
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
func publishViaRelease(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool, chName, target string, makePub func([]build.Artifact) channel.Publisher) output.Result {
	// 生成物(goreleaser.yaml ＋ script 有効なら install.sh)を .wharfy/ に書く(03)。
	configPath, err := writeGeneratedConfig(root, cfg, in, version)
	if err != nil {
		return internalError(c, err)
	}

	if !flagYes {
		// preview: snapshot でローカルに archive を作り(アップロードしない)、暫定 sha で差分を見せる。
		archs, aerr := newArchiver(config.DistDir).Archives(ctx, root, configPath)
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
	archs, _, rerr := releaseArtifacts(ctx, root, configPath, version)
	if rerr != nil {
		return buildErrorResult(c, rerr)
	}
	return ownedReleaseApply(ctx, c, makePub(archs), root, cfg.Project, chName, target, cfg.Github, version)
}

// releaseArtifacts は publish の apply で使う成果物を返す。release(同 version)が記録済みなら
// 再アップロードせず再利用し(reused=true)、無ければ release パスを走らせて記録する(後方互換)。
func releaseArtifacts(ctx context.Context, root, configPath, version string) ([]build.Artifact, bool, error) {
	if set, found, _ := build.LoadArtifacts(root); found && set.Version == version {
		return set.Artifacts, true, nil
	}
	archs, err := newReleaser(config.DistDir).Release(ctx, root, configPath)
	if err != nil {
		return nil, false, err
	}
	_ = build.SaveArtifacts(root, version, archs) // 後続の publish <ch> が再利用できるよう記録
	return archs, false, nil
}

func ownedSkip(c registry.Command, chName, reason string) output.Result {
	item := channel.PlanItem{Channel: chName, Kind: channel.KindOwned, Action: channel.ActionSkip, Reason: reason}
	res := publishResult(c, chName+" skipped — unresolved target", true, []channel.PlanItem{item})
	res.Next = []output.NextDo{{Reason: "check the resolved config", Do: "wharfy config"}}
	return res
}

// publishScript は script チャネル(curl|sh インストーラ・03/07)。install.sh を生成し、
// 実 release の extra_files で同梱アップロードする。書く前に install.sh の内容を見せる。
func publishScript(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool) output.Result {
	if cfg.Github == "" {
		item := channel.PlanItem{Channel: "script", Kind: channel.KindOwned, Action: channel.ActionSkip,
			Reason: "github unresolved — install.sh needs the release repo"}
		res := publishResult(c, "script skipped — github unresolved", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "set github so the release can be derived", Do: "wharfy config"}}
		return res
	}

	script := config.GenerateInstallScript(cfg, version)
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
	// 実 release: archive ＋ install.sh(extra_files)を GitHub Releases へアップロード。
	if _, rerr := newReleaser(config.DistDir).Release(ctx, root, configPath); rerr != nil {
		return buildErrorResult(c, rerr)
	}
	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		now := nowUTC().Format(time.RFC3339)
		st.Publish["releases"] = state.PublishRecord{Version: version, Target: cfg.Github, At: now}
		st.Publish["script"] = state.PublishRecord{Version: version, Target: cfg.Github + " release:" + config.InstallScriptName, At: now}
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

// publishGoinstall は goinstall チャネル(梱包ゼロ・03/07)。何も push せず、module proxy で
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

// homebrewPublisher / scoopPublisher は archive から各 Publisher を組む。
func homebrewPublisher(cfg config.Config, in config.File, tap, tapOwner, tapRepo, ghOwner, ghRepo, version string, archs []build.Artifact) *channel.Homebrew {
	return &channel.Homebrew{
		Project: cfg.Project,
		Tap:     tap,
		Store:   newTapStore(tapOwner, tapRepo, os.Getenv("GITHUB_TOKEN")),
		Input: channel.FormulaInput{
			Project:     cfg.Project,
			Description: in.Description,
			Homepage:    cfg.Homepage,
			License:     cfg.License,
			Version:     version,
			Archives:    formulaArchives(archs, ghOwner, ghRepo, cfg.Project, version),
		},
	}
}

func scoopPublisher(cfg config.Config, in config.File, bucket, bOwner, bRepo, ghOwner, ghRepo, version string, archs []build.Artifact) *channel.Scoop {
	return &channel.Scoop{
		Project: cfg.Project,
		Bucket:  bucket,
		Store:   newTapStore(bOwner, bRepo, os.Getenv("GITHUB_TOKEN")),
		Input: channel.ScoopInput{
			Project:     cfg.Project,
			Description: in.Description,
			Homepage:    cfg.Homepage,
			License:     cfg.License,
			Version:     version,
			Owner:       ghOwner,
			Repo:        ghRepo,
			Archives:    scoopArchives(archs, ghOwner, ghRepo, cfg.Project, version),
		},
	}
}

// ownedReleaseDryRun は plan をプレビューする(書かない)。requires で実 apply の前提条件を先出し。
func ownedReleaseDryRun(ctx context.Context, c registry.Command, pub channel.Publisher, version, chName, target string, tagMissing bool) output.Result {
	item, err := pub.Plan(ctx)
	if err != nil {
		return probeFailedResult(c, err)
	}
	reqs := applyRequirements(tagMissing)
	msg := "plan: " + item.Action + " " + item.OwnedArtifact
	msg += previewNote(version, tagMissing, true)
	res := output.New(c.Name, msg, true)
	res.Data = publishData{Applied: false, Plan: []channel.PlanItem{item}, Requires: reqs}
	res.Next = dryRunNext(item, reqs, chName)
	// 自前リポジトリ(tap/bucket)が未作成なら予告する(--yes で wharfy が作る・ADR-8)。
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
	for _, r := range reqs {
		if !r.Met {
			next = append(next, output.NextDo{Reason: "required before --yes: " + r.Requirement, Do: r.Hint})
		}
	}
	next = append(next, output.NextDo{Reason: "apply the shown changes", Do: "wharfy publish " + chName + " --yes"})
	return next
}

// writeGeneratedConfig は所有する生成物(goreleaser.yaml ＋ script 有効時は install.sh)を
// .wharfy/ に書く(03)。install.sh は extra_files が参照するので、生成設定と必ず同時に書く。
func writeGeneratedConfig(root string, cfg config.Config, in config.File, version string) (string, error) {
	glYAML, err := config.GenerateGoReleaser(cfg, in)
	if err != nil {
		return "", err
	}
	if config.HasChannel(cfg, "script") {
		if _, err := config.WriteInstallScript(root, config.GenerateInstallScript(cfg, version)); err != nil {
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

// tagMissingResult / tokenMissingResult は実 apply の前提不足(09)。実リリース前に弾く。
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
func ownedReleaseApply(ctx context.Context, c registry.Command, pub channel.Publisher, root, project, chName, target, releaseTarget, version string) output.Result {
	// 自前リポジトリ(tap/bucket)が無ければ作る(--yes の明示同意があるので・ADR-8/03)。
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

	if st, err := state.Load(root, project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		now := nowUTC().Format(time.RFC3339)
		// releases(archive アップロード)とチャネル(formula/manifest)の両方を記録する。
		st.Publish["releases"] = state.PublishRecord{Version: version, Target: releaseTarget, At: now}
		st.Publish[chName] = state.PublishRecord{Version: version, Target: target, Commit: pubres.Commit, At: now}
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

func probeFailedResult(c registry.Command, err error) output.Result {
	res := output.New(c.Name, "cannot read the tap", false)
	res.Errors = []output.Problem{{Code: output.ErrProbeFailed, Message: err.Error(), Hint: "check network or tap visibility"}}
	res.Next = []output.NextDo{{Reason: "retry once reachable", Do: "wharfy publish homebrew --dry-run"}}
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

// formulaArchives は build の archive(darwin/linux)を Releases の URL 付き ArchiveRef にする。
func formulaArchives(archs []build.Artifact, ghOwner, ghRepo, project, version string) []channel.ArchiveRef {
	var out []channel.ArchiveRef
	for _, a := range archs {
		if a.OS != "darwin" && a.OS != "linux" {
			continue // homebrew は darwin/linux のみ
		}
		name := fmt.Sprintf("%s_%s_%s_%s.tar.gz", project, version, a.OS, a.Arch)
		url := fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", ghOwner, ghRepo, version, name)
		out = append(out, channel.ArchiveRef{OS: a.OS, Arch: a.Arch, URL: url, SHA256: a.SHA256})
	}
	channel.SortArchives(out)
	return out
}

func splitOwnerName(s string) (owner, name string, ok bool) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
