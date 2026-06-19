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
	// goinstallProxy はテストで module proxy を httptest に差し替える点(空＝既定 proxy)。
	goinstallProxy = ""
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
	case "goinstall":
		return publishGoinstall(ctx, c, root, cfg, tagMissing)
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

// publishHomebrew は homebrew チャネルの発行(差分プレビュー→実 release+formula)。
func publishHomebrew(ctx context.Context, c registry.Command, root string, cfg config.Config, in config.File, version string, tagMissing bool) output.Result {
	tap, ok := homebrewTarget(cfg)
	tapOwner, tapRepo, tapOK := splitOwnerName(tap)
	ghOwner, ghRepo, ghOK := splitOwnerName(cfg.Github)
	if !ok || !tapOK || !ghOK {
		item := channel.PlanItem{
			Channel: "homebrew", Kind: channel.KindOwned, Action: channel.ActionSkip,
			Reason: "homebrew tap/github unresolved — set 'github' or 'homebrew.tap' in wharfy.yaml",
		}
		res := publishResult(c, "homebrew skipped — tap unresolved", true, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "set github so the tap can be derived", Do: "wharfy config"}}
		return res
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

	ctx2 := publishCtx{cfg: cfg, in: in, tap: tap, tapOwner: tapOwner, tapRepo: tapRepo, ghOwner: ghOwner, ghRepo: ghRepo, version: version}

	if !flagYes {
		// preview: snapshot でローカルに archive を作り(アップロードしない)、暫定 sha で差分を見せる。
		archs, aerr := newArchiver(config.DistDir).Archives(ctx, root, configPath)
		if aerr != nil {
			return buildErrorResult(c, aerr)
		}
		return publishDryRun(c, ctx2.homebrew(archs), ctx, tagMissing)
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
	return publishApply(c, ctx2.homebrew(archs), ctx, root, cfg, tap, version)
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

// publishCtx は publish の解決済みコンテキスト(homebrew Publisher を archive から組む)。
type publishCtx struct {
	tap, tapOwner, tapRepo, ghOwner, ghRepo string
	version                                 string
	cfg                                     config.Config
	in                                      config.File
}

func (p publishCtx) homebrew(archs []build.Artifact) *channel.Homebrew {
	return &channel.Homebrew{
		Project: p.cfg.Project,
		Tap:     p.tap,
		Store:   newTapStore(p.tapOwner, p.tapRepo, os.Getenv("GITHUB_TOKEN")),
		Input: channel.FormulaInput{
			Project:     p.cfg.Project,
			Description: p.in.Description,
			Homepage:    p.cfg.Homepage,
			License:     p.cfg.License,
			Version:     p.version,
			Archives:    formulaArchives(archs, p.ghOwner, p.ghRepo, p.cfg.Project, p.version),
		},
	}
}

// publishDryRun は plan をプレビューする(書かない)。
// requires で実 apply の前提条件(tag/token)とその充足状況を先に見せる。
func publishDryRun(c registry.Command, hb *channel.Homebrew, ctx context.Context, tagMissing bool) output.Result {
	item, err := hb.Plan(ctx)
	if err != nil {
		return probeFailedResult(c, err)
	}
	reqs := applyRequirements(tagMissing)

	msg := "plan: " + item.Action + " " + hb.FormulaPath()
	if tagMissing {
		// 正準コードに合う warning が無いので、誤コードを付けず message で正直に注記する。
		msg += " (preview @ " + hb.Input.Version + "; no git tag yet)"
	}
	res := output.New(c.Name, msg, true)
	res.Data = publishData{Applied: false, Plan: []channel.PlanItem{item}, Requires: reqs}
	res.Next = dryRunNext(item, reqs)
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
func dryRunNext(item channel.PlanItem, reqs []requirement) []output.NextDo {
	if item.Action == channel.ActionNoop {
		return []output.NextDo{{Reason: "already up to date; verify install", Do: "wharfy verify"}}
	}
	next := []output.NextDo{}
	for _, r := range reqs {
		if !r.Met {
			next = append(next, output.NextDo{Reason: "required before --yes: " + r.Requirement, Do: r.Hint})
		}
	}
	next = append(next, output.NextDo{Reason: "apply the shown changes to the tap", Do: "wharfy publish homebrew --yes"})
	return next
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

// publishApply は実 archive 反映後に formula を tap に書く(--yes)。前提(tag/token)は確認済み。
// archive は既に GitHub Releases へアップロード済み(Releaser.Release)。formula は実 checksum を持つ。
func publishApply(c registry.Command, hb *channel.Homebrew, ctx context.Context, root string, cfg config.Config, tap, version string) output.Result {
	item, pub, err := hb.Publish(ctx)
	if err != nil {
		res := output.New(c.Name, "publish failed", false)
		res.Errors = []output.Problem{{Code: output.ErrPublishFailed, Message: err.Error(), Hint: "check token scope and tap permissions"}}
		res.Next = []output.NextDo{{Reason: "fix the cause then retry", Do: "wharfy publish homebrew --yes"}}
		return res
	}

	if st, err := state.Load(root, cfg.Project); err == nil {
		if st.Publish == nil {
			st.Publish = map[string]state.PublishRecord{}
		}
		now := nowUTC().Format(time.RFC3339)
		// releases(archive アップロード)と homebrew(formula)の両方を記録する。
		st.Publish["releases"] = state.PublishRecord{Version: version, Target: cfg.Github, At: now}
		st.Publish["homebrew"] = state.PublishRecord{Version: version, Target: tap, Commit: pub.Commit, At: now}
		_ = state.Save(root, st)
	}

	item.Action = channel.ActionUpdate // 反映済みの操作を明示(create/update いずれも書いた)
	res := publishResult(c, "published "+hb.Project+" "+version+" → "+tap, true, []channel.PlanItem{item})
	res.Data = publishData{Applied: true, Plan: []channel.PlanItem{item}}
	res.Next = []output.NextDo{{Reason: "install from the tap and run it", Do: "wharfy verify"}}
	return res
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
