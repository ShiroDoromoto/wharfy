package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
	"github.com/ShiroDoromoto/wharfy/internal/state"
)

// verify.go — wharfy.yaml の channels: にあるチャネルを消費側から確かめる。
//
// 検証の対象集合は `channels:` が決める(D-4)。state.json の publish 履歴は監査記録であって
// 行動の根拠にしない — 設定から外したチャネルの古い記録が残っていても検証しない。畳んだ tap を
// 検証して緑を返し、現行チャネルを一つも見ていない、という嘘をつかないため。
//
// homebrew は自前 tap の formula の有無と版を照合する。apt/rpm はそれに加えて Linux コンテナで
// repo を足し、install して実行まで踏む。供給側(hosted repo への push)はアップロードが 200 を返せば
// 成功するので、生成した deb/rpm の依存やファイル配置が壊れていても気づけない。踏むのは利用者になる。
//
// `wharfy verify [channel]` は対象を 1 チャネルに絞る。channels: に無い名前は publish と同じ
// channel_not_configured で拒む。
//
// docker が無ければ apt/rpm のコンテナ検証だけを skip する(docker 不在は verify の失敗ではない)。
// 一つも検証できなかったときは ok=false(nothing_to_verify)。「確かめられなかった」を緑で返すと、
// CI がそれを「配布は健全」と読んでしまう(D-4)。

// verifyCheck は 1 チャネル分の検証結果(verify の data)。
type verifyCheck struct {
	Channel string `json:"channel"`
	Status  string `json:"status"` // verified / partial / failed / skipped
	Message string `json:"message"`
}

// verifyData は verify の data ペイロード。
type verifyData struct {
	Checks []verifyCheck `json:"checks"`
}

const (
	verifyStatusOK = "verified"
	// verifyStatusPartial は「検証は走ったが、最後まで踏めなかった」(例: repo の版は照合したが
	// docker が無く install を試せない)。失敗ではないので ok は落とさないが、verified とも呼ばない。
	// 検証対象ゼロ(nothing_to_verify)の判定では「走った」側に数える。
	verifyStatusPartial = "partial"
	verifyStatusFailed  = "failed"
	verifyStatusSkipped = "skipped"
)

// verifyOutcome は 1 チャネルの検証結果と、それが呼ぶ envelope 要素(問題・警告・次の一手)。
type verifyOutcome struct {
	check   verifyCheck
	problem *output.Problem
	warning *output.Warning
	next    *output.NextDo
}

var (
	// dockerRun はコンテナ実行の末端(テストで差し替え)。CombinedOutput を返す。
	dockerRun = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	}
	// verifyImages は apt/rpm を確かめるベースイメージ。利用者の環境の代表として debian/fedora を踏む。
	verifyImages = map[string]string{"apt": "debian:12", "rpm": "fedora:40"}
	// verifyTimeout はコンテナ 1 本の上限。apt-get update / dnf のメタデータ取得は遅い。
	verifyTimeout = 10 * time.Minute
)

// runVerify は channels: にあるチャネルの到達性・整合性を確認する(verify)。
// 引数でチャネルを1つ名指しできる(省略時は channels: の全部)。apt/rpm はコンテナを起こすので、
// 1 チャネルを直している間は他を走らせない方が反復が軽い。
// 未発行・未対応のチャネルは skip として checks に載せ、一つも検証できなければ ok=false を返す。
func runVerify(ctx context.Context, c registry.Command, args []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}
	in, _ := config.Load(root)
	cfg, _ := config.NewResolver(root).Resolve(in)
	st, _ := state.Load(root, cfg.Project)

	targets := cfg.Channels
	if len(args) > 0 {
		sel, ok := selectChannel(cfg, args[0])
		if !ok {
			return verifyChannelNotConfigured(c, cfg, in, args[0])
		}
		targets = []config.ResolvedChannel{sel}
	}

	var outcomes []verifyOutcome
	var unpublished []string
	for _, ch := range targets {
		rec, published := publishedRecord(st, ch.Name)
		if !published {
			unpublished = append(unpublished, ch.Name)
			outcomes = append(outcomes, verifyNotRun(ch.Name, ch.Name+" skipped: nothing published on this channel yet"))
			continue
		}
		switch ch.Name {
		case "homebrew":
			oc, err := verifyHomebrew(ctx, cfg, ch, rec)
			if err != nil {
				return internalError(c, err)
			}
			outcomes = append(outcomes, oc)
		case "apt", "rpm":
			outcomes = append(outcomes, verifyLinuxRepo(ctx, ch, cfg, in, rec))
		default:
			outcomes = append(outcomes, verifyNotRun(ch.Name, ch.Name+" skipped: verify does not cover this channel yet"))
		}
	}
	return verifyResult(c, outcomes, unpublished)
}

// selectChannel は名指しされたチャネルを channels: から引く。
func selectChannel(cfg config.Config, name string) (config.ResolvedChannel, bool) {
	for _, ch := range cfg.Channels {
		if ch.Name == name {
			return ch, true
		}
	}
	return config.ResolvedChannel{}, false
}

// verifyChannelNotConfigured は名指しされたチャネルが channels: に無いときの拒否。publish と同じ
// 語彙で断る(D-4: 宣言した集合だけが対象)。畳んだチャネルを検証して緑を返さないため。
//
// publish は「未実装チャネル」へのディスパッチを持つので綴り違いをそこで拾うが、verify の対象は
// 常に channels: 由来なので、綴り違いも「設定に無い」に落ちる。直し方だけ hint で分ける
// ——書き戻せと言われても、そんな名前のチャネルは存在しない。
func verifyChannelNotConfigured(c registry.Command, cfg config.Config, in config.File, name string) output.Result {
	res := output.New(c.Name, name+" is not in wharfy.yaml channels: — nothing verified", false)
	res.Data = verifyData{Checks: []verifyCheck{}}
	prob := channelNotConfiguredProblem(cfg, in, name)
	if !config.KnownChannel(name) {
		prob.Hint = "there is no wharfy channel named '" + name + "' — check the spelling"
	}
	res.Errors = []output.Problem{prob}
	res.Next = []output.NextDo{{Reason: "verify the channels you declared", Do: "wharfy verify"}}
	return res
}

// publishedRecord は state から発行済みの記録を返す(版が空なら未発行扱い)。
func publishedRecord(st *state.State, name string) (state.PublishRecord, bool) {
	rec, has := st.Publish[name]
	if !has || rec.Version == "" {
		return state.PublishRecord{}, false
	}
	return rec, true
}

// verifyResult は各チャネルの結果を 1 つの envelope に畳む。
//
//	failed が 1 つでもあれば ok=false。
//	検証が 1 つも走らなければ ok=false(nothing_to_verify) — skip だけで緑を返さない。
//	partial は「走ったが最後まで踏めなかった」なので、ok を落とさず対象ゼロにも数えない。
//
// unpublished は channels: にあるが未発行のチャネル(検証対象ゼロのときの次の一手に使う)。
func verifyResult(c registry.Command, outcomes []verifyOutcome, unpublished []string) output.Result {
	var checks []verifyCheck
	var problems []output.Problem
	var warnings []output.Warning
	var nexts []output.NextDo
	failed, exercised := false, 0
	for _, oc := range outcomes {
		checks = append(checks, oc.check)
		switch oc.check.Status {
		case verifyStatusFailed:
			failed = true
		case verifyStatusOK, verifyStatusPartial:
			exercised++
		}
		if oc.problem != nil {
			problems = append(problems, *oc.problem)
		}
		if oc.warning != nil {
			warnings = append(warnings, *oc.warning)
		}
		if oc.next != nil {
			nexts = append(nexts, *oc.next)
		}
	}

	nothingToVerify := !failed && exercised == 0
	res := output.New(c.Name, verifyMessage(checks, nothingToVerify), !failed && !nothingToVerify)
	res.Data = verifyData{Checks: checks}
	res.Warnings = warnings
	res.Errors = problems
	switch {
	case failed:
		res.Next = nexts
	case nothingToVerify:
		res.Errors = append(res.Errors, output.Problem{
			Code:    output.ErrNothingToVerify,
			Message: "no channel in wharfy.yaml could be verified",
			Hint:    "publish a channel that verify covers, or check why every channel was skipped",
		})
		res.Next = nothingToVerifyNext(unpublished)
	default:
		res.Next = []output.NextDo{{Reason: "distribution looks consistent; review overall state", Do: "wharfy status"}}
	}
	return res
}

// nothingToVerifyNext は検証対象ゼロのときの次の一手。channels: にあるチャネルだけを勧める
// (設定から外したチャネルへの publish を勧めない・D-4)。
func nothingToVerifyNext(unpublished []string) []output.NextDo {
	if len(unpublished) > 0 {
		return []output.NextDo{{Reason: "publish first, then verify the install", Do: "wharfy publish " + unpublished[0] + " --yes"}}
	}
	return []output.NextDo{{Reason: "verify covers none of the configured channels yet; review overall state", Do: "wharfy status"}}
}

// verifyMessage は「何が通り、何が落ち、何を飛ばしたか」を一行にする。
func verifyMessage(checks []verifyCheck, nothingToVerify bool) string {
	byStatus := map[string][]string{}
	for _, ck := range checks {
		byStatus[ck.Status] = append(byStatus[ck.Status], ck.Channel)
	}
	var parts []string
	for _, st := range []string{verifyStatusFailed, verifyStatusOK, verifyStatusPartial, verifyStatusSkipped} {
		if names := byStatus[st]; len(names) > 0 {
			parts = append(parts, st+" "+strings.Join(names, ", "))
		}
	}
	if !nothingToVerify {
		return strings.Join(parts, "; ")
	}
	if len(parts) == 0 {
		return "nothing to verify: no channels in wharfy.yaml"
	}
	return "nothing to verify: " + strings.Join(parts, "; ")
}

// verifyHomebrew は自前 tap の formula が在り、版が記録と一致するかを照合する。
// tap は設定の解決値を先に採る(記録は監査用のフォールバック・D-4)。どちらでも解決できないのは
// 設定の壊れ(検証の失敗ではない)なので error で返す。
func verifyHomebrew(ctx context.Context, cfg config.Config, ch config.ResolvedChannel, rec state.PublishRecord) (verifyOutcome, error) {
	tap := firstNonEmptyStr(ch.Target, rec.Target)
	owner, repo, ok := splitOwnerName(tap)
	if !ok {
		return verifyOutcome{}, errString("homebrew target is unresolved: " + tap)
	}
	hb := &channel.Homebrew{
		Project: cfg.Project,
		Tap:     tap,
		Store:   newTapStore(owner, repo, os.Getenv("GITHUB_TOKEN")),
	}
	rs, perr := hb.Probe(ctx)
	if perr != nil {
		return probeFailedOutcome("homebrew", perr), nil
	}
	switch {
	case !rs.Found:
		return verifyFailure("homebrew",
			"homebrew recorded "+rec.Version+" but no formula at "+tap,
			"published formula not found on the tap",
			"re-publish to restore the formula",
			"", "wharfy publish homebrew --yes"), nil
	case rs.Version != rec.Version:
		return verifyFailure("homebrew",
			"tap has "+rs.Version+", expected "+rec.Version,
			"tap formula version does not match the published record",
			"re-publish to align the tap with the recorded version",
			"", "wharfy publish homebrew --yes"), nil
	default:
		return verifySuccess("homebrew", "homebrew "+rs.Version+" verified: formula present at "+tap+", version matches record"), nil
	}
}

// verifyLinuxRepo は apt/rpm を二段で確かめる。
//
//  1. hosted repo のメタデータに記録どおりの版が載っているか(供給側)。
//  2. その repo を足したコンテナで install し、入ったバイナリが動くか(消費側)。
//
// 2 が無いと、依存不足やパス誤りのパッケージをアップロード成功のまま配ってしまう。
func verifyLinuxRepo(ctx context.Context, ch config.ResolvedChannel, cfg config.Config, in config.File, rec state.PublishRecord) verifyOutcome {
	name := ch.Name
	repo := firstNonEmptyStr(ch.Target, rec.Target)
	if repo == "" {
		return verifySkip(name, name+" skipped: repo is unresolved in the config")
	}
	pkg, binary := cfg.Project, prebuiltBinaryName(cfg, in)

	rs, perr := probeLinuxRepo(ctx, name, repo, pkg)
	if perr != nil {
		return probeFailedOutcome(name, perr)
	}
	switch {
	case !rs.Found:
		return verifyFailure(name,
			name+" recorded "+rec.Version+" but no package at "+repo,
			"published package not found in the repo",
			"re-publish to restore the package",
			"", "wharfy publish "+name+" --yes")
	case rs.Version != rec.Version:
		return verifyFailure(name,
			name+" repo has "+rs.Version+", expected "+rec.Version,
			"repo package version does not match the published record",
			"re-publish to align the repo with the recorded version",
			"", "wharfy publish "+name+" --yes")
	}

	if !dockerAvailable() {
		return verifyPartial(name, name+" "+rs.Version+" found in "+repo+", but the install was not exercised: docker is not available")
	}
	image := verifyImages[name]
	out, err := containerInstall(ctx, name, repo, pkg, binary)
	if err != nil {
		return verifyFailure(name,
			name+" "+rs.Version+" is in the repo but installing it in "+image+" failed",
			"install from the repo failed: "+err.Error(),
			"read the container output; the package's dependencies or file layout are likely wrong",
			tail(out, 4000), "wharfy publish "+name+" --yes")
	}
	return verifySuccess(name, name+" "+rs.Version+" verified: installed from "+repo+" in "+image+" and ran")
}

// probeLinuxRepo は hosted repo のメタデータから pkg の最新版を読む(status と同じ照合器)。
func probeLinuxRepo(ctx context.Context, name, repo, pkg string) (channel.RemoteState, error) {
	if name == "rpm" {
		return (&channel.RpmProbe{Repo: repo}).Probe(ctx, pkg)
	}
	return (&channel.AptProbe{Repo: repo}).Probe(ctx, pkg)
}

// containerInstall は使い捨てコンテナで repo を足し、install → 実行まで走らせる。
// 出力は失敗時の診断に返す(成功しても捨てない)。
func containerInstall(ctx context.Context, name, repo, pkg, binary string) ([]byte, error) {
	if err := checkShellSafe(repo, pkg, binary); err != nil {
		return nil, err
	}
	script := aptVerifyScript(repo, pkg, binary)
	if name == "rpm" {
		script = rpmVerifyScript(repo, pkg, binary)
	}
	cctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	return dockerRun(cctx, "run", "--rm", verifyImages[name], "bash", "-lc", script)
}

// aptVerifyScript は debian 系コンテナで repo を足し、install して実行するシェル。
//
// trusted=yes で署名検証を外す。ここで見たいのは「パッケージが入って動くか」であり、
// 鍵を配れているかは repo ホスト側の話なので切り離す。
// 実行は --version → version → --help の順に試し、どれか 1 つが通れば「動いた」とみなす
// (サブコマンドの名前はプロジェクトによる。依存不足やパス誤りならどれも起動しない)。
func aptVerifyScript(repo, pkg, binary string) string {
	return fmt.Sprintf(`set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates
echo 'deb [trusted=yes] %s /' > /etc/apt/sources.list.d/wharfy-verify.list
apt-get update -qq
apt-get install -y -qq %s
command -v %s
%s --version || %s version || %s --help
`, repo, pkg, binary, binary, binary, binary)
}

// rpmVerifyScript は fedora 系コンテナで repo を足し、install して実行するシェル。
func rpmVerifyScript(repo, pkg, binary string) string {
	return fmt.Sprintf(`set -eu
cat > /etc/yum.repos.d/wharfy-verify.repo <<'EOF'
[wharfy-verify]
name=wharfy verify
baseurl=%s
enabled=1
gpgcheck=0
EOF
dnf install -y -q %s
command -v %s
%s --version || %s version || %s --help
`, repo, pkg, binary, binary, binary, binary)
}

// shellSafeName はコンテナ内のシェルにそのまま渡してよい名前(パッケージ名・バイナリ名)。
var shellSafeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// checkShellSafe は設定由来の値をシェルスクリプトへ埋める前に検査する。
// wharfy.yaml は利用者のものだが、書き間違いがコンテナ内で任意コマンドになるのは事故なので断る。
func checkShellSafe(repo, pkg, binary string) error {
	if !strings.HasPrefix(repo, "http://") && !strings.HasPrefix(repo, "https://") {
		return errString("repo must be an http(s) url to verify in a container: " + repo)
	}
	if strings.ContainsAny(repo, " \t\n\r'\"`$\\") {
		return errString("repo url cannot be passed to a shell safely: " + repo)
	}
	for _, s := range []string{pkg, binary} {
		if !shellSafeName.MatchString(s) {
			return errString("name cannot be passed to a shell safely: " + s)
		}
	}
	return nil
}

// verifySuccess / verifyPartial / verifyFailure / verifySkip / verifyNotRun / probeFailedOutcome は
// 1 チャネル分の結果を組む。
func verifySuccess(name, msg string) verifyOutcome {
	return verifyOutcome{check: verifyCheck{Channel: name, Status: verifyStatusOK, Message: msg}}
}

// verifyPartial は検証の一部だけを踏めたチャネル。踏めなかった事実を warning に残す
// (配布者は docker を入れれば最後まで確かめられる)。
func verifyPartial(name, msg string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusPartial, Message: msg},
		warning: &output.Warning{Code: output.WarnChannelSkipped, Message: msg},
	}
}

func verifyFailure(name, msg, problem, hint, detail, next string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusFailed, Message: msg},
		problem: &output.Problem{Code: output.ErrVerifyFailed, Message: problem, Hint: hint, Detail: detail},
		next:    &output.NextDo{Reason: "re-publish " + name + " to fix what verify found", Do: next},
	}
}

// verifySkip は検証を飛ばした事実を warning として残す。ok は落とさないが、黙って通してもいない。
// 配布者が手を打てる skip(docker を入れる・repo を設定する)だけがここを通る。
func verifySkip(name, msg string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusSkipped, Message: msg},
		warning: &output.Warning{Code: output.WarnChannelSkipped, Message: msg},
	}
}

// verifyNotRun は検証を走らせなかったチャネル(未発行・verify が未対応)。warning は出さない
// ——配布者に打てる手が無いか、publish していないという既知の事実だからで、checks には載る。
// 全チャネルがこれなら verifyResult が nothing_to_verify として ok=false にする。
func verifyNotRun(name, msg string) verifyOutcome {
	return verifyOutcome{check: verifyCheck{Channel: name, Status: verifyStatusSkipped, Message: msg}}
}

func probeFailedOutcome(name string, err error) verifyOutcome {
	msg := "cannot reach " + name + " to verify: " + err.Error()
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusFailed, Message: msg},
		problem: &output.Problem{Code: output.ErrProbeFailed, Message: err.Error(), Hint: "check network or channel visibility"},
		next:    &output.NextDo{Reason: "retry once reachable", Do: "wharfy verify"},
	}
}

// tail は診断用に出力の末尾 n バイトを返す。
func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

type errString string

func (e errString) Error() string { return string(e) }
