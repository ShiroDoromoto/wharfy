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
	newTapStore = func(owner, repo, token string) channel.TapStore {
		return channel.NewGitHubTapStore(owner, repo, token)
	}
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
// スライス1 は homebrew のみ実装。他チャネルは plan で skip を返す(型は同一)。
func runPublish(ctx context.Context, c registry.Command, args []string) output.Result {
	ch := "homebrew"
	if len(args) > 0 {
		ch = args[0]
	}
	if ch != "homebrew" {
		item := channel.PlanItem{
			Channel: ch, Action: channel.ActionSkip,
			Reason: "slice 1 supports the 'homebrew' channel only",
		}
		res := publishResult(c, "channel "+ch+" not implemented yet", false, []channel.PlanItem{item})
		res.Next = []output.NextDo{{Reason: "publish the supported channel", Do: "wharfy publish homebrew"}}
		return res
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

	version, tagMissing := publishVersion(root)

	// archive(formula の sha256 用)を release-snapshot で生成。生成設定は .wharfy/ に書く(03)。
	glYAML, err := config.GenerateGoReleaser(cfg, in)
	if err != nil {
		return internalError(c, err)
	}
	configPath, err := config.WriteGoReleaser(root, glYAML)
	if err != nil {
		return internalError(c, err)
	}
	archs, aerr := newArchiver(config.DistDir).Archives(ctx, root, configPath)
	if aerr != nil {
		return buildErrorResult(c, aerr) // builder_unavailable / build_failed を流用
	}

	hb := &channel.Homebrew{
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

	if !flagYes {
		return publishDryRun(c, hb, ctx, tagMissing)
	}
	return publishApply(c, hb, ctx, root, cfg, tap, version, tagMissing)
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

// publishApply は実際に tap に書く(--yes)。tag/token が要る。
func publishApply(c registry.Command, hb *channel.Homebrew, ctx context.Context, root string, cfg config.Config, tap, version string, tagMissing bool) output.Result {
	if tagMissing {
		res := output.New(c.Name, "cannot publish without a tag", false)
		res.Errors = []output.Problem{{Code: output.ErrTagMissing, Message: "no git tag found; the tag is the version", Hint: "git tag vX.Y.Z && git push --tags, then retry"}}
		res.Next = []output.NextDo{{Reason: "tag the release", Do: "git tag v" + version}}
		return res
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		res := output.New(c.Name, "cannot publish without a token", false)
		res.Errors = []output.Problem{{Code: output.ErrTokenMissing, Message: "GITHUB_TOKEN required to write the tap", Hint: "export GITHUB_TOKEN=…"}}
		res.Next = []output.NextDo{{Reason: "set the token then retry", Do: "export GITHUB_TOKEN=… ; wharfy publish homebrew --yes"}}
		return res
	}

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
		st.Publish["homebrew"] = state.PublishRecord{
			Version: version, Target: tap, Commit: pub.Commit, At: nowUTC().Format(time.RFC3339),
		}
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
