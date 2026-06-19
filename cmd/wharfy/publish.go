package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	newArchiver = func(distDir string) build.Archiver { return build.NewGoReleaserBuilder(distDir) }
	newReleaser = func(distDir string) build.Releaser { return build.NewGoReleaserBuilder(distDir) }
	newTapStore = func(owner, repo, token string) channel.TapStore {
		return channel.NewGitHubTapStore(owner, repo, token)
	}
	// goinstallProxy / scriptProbeURL はテストで実体照合先を httptest に差し替える点(空＝既定)。
	goinstallProxy = ""
	scriptProbeURL = ""
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
	ch := "homebrew"
	if len(args) > 0 {
		ch = args[0]
	}

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

	switch ch {
	case "homebrew":
		return publishHomebrew(ctx, c, root, cfg, in, version, tagMissing)
	case "scoop":
		return publishScoop(ctx, c, root, cfg, in, version, tagMissing)
	case "goinstall":
		return publishGoinstall(ctx, c, root, cfg, tagMissing)
	case "script":
		return publishScript(ctx, c, root, cfg, in, version, tagMissing)
	default:
		item := channel.PlanItem{
			Channel: ch, Action: channel.ActionSkip,
			Reason: "channel not implemented yet (supported: homebrew, goinstall)",
		}
		res := publishResult(c, "channel "+ch+" not implemented yet", false, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "publish a supported channel", Do: "wharfy publish homebrew"}}
		return res
	}
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
		return ownedReleaseDryRun(ctx, c, makePub(archs), version, chName, tagMissing)
	}

	// apply: 高コストな実リリースの前に前提を確認する(tag / token)。
	if tagMissing {
		return tagMissingResult(c, version)
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return tokenMissingResult(c)
	}
	// 実リリース: archive を GitHub Releases へアップロードし、実 sha256 を得る(--skip=homebrew)。
	archs, rerr := newReleaser(config.DistDir).Release(ctx, root, configPath)
	if rerr != nil {
		return buildErrorResult(c, rerr)
	}
	return ownedReleaseApply(ctx, c, makePub(archs), root, cfg.Project, chName, target, cfg.Github, version)
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
func ownedReleaseDryRun(ctx context.Context, c registry.Command, pub channel.Publisher, version, chName string, tagMissing bool) output.Result {
	item, err := pub.Plan(ctx)
	if err != nil {
		return probeFailedResult(c, err)
	}
	reqs := applyRequirements(tagMissing)
	msg := "plan: " + item.Action + " " + item.OwnedArtifact
	if tagMissing {
		// 正準コードに合う warning が無いので、誤コードを付けず message で正直に注記する。
		msg += " (preview @ " + version + "; no git tag yet)"
	}
	res := output.New(c.Name, msg, true)
	res.Data = publishData{Applied: false, Plan: []channel.PlanItem{item}, Requires: reqs}
	res.Next = dryRunNext(item, reqs, chName)
	return res
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
