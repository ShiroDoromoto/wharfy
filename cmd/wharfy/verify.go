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

// verify.go — 発行済み owned チャネルを消費側から確かめる。
//
// homebrew は自前 tap の formula の有無と版を照合する。apt/rpm はそれに加えて Linux コンテナで
// repo を足し、install して実行まで踏む。供給側(hosted repo への push)はアップロードが 200 を返せば
// 成功するので、生成した deb/rpm の依存やファイル配置が壊れていても気づけない。踏むのは利用者になる。
//
// docker が無ければ apt/rpm のコンテナ検証だけを skip する(docker 不在は verify の失敗ではない)。

// verifyCheck は 1 チャネル分の検証結果(verify の data)。
type verifyCheck struct {
	Channel string `json:"channel"`
	Status  string `json:"status"` // verified / failed / skipped
	Message string `json:"message"`
}

// verifyData は verify の data ペイロード。
type verifyData struct {
	Checks []verifyCheck `json:"checks"`
}

const (
	verifyStatusOK      = "verified"
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

// runVerify は発行済み owned チャネルの到達性・整合性を確認する(verify)。
// 未発行なら「確認対象なし」を正直に返し、publish を促す(空 next の dead-end を作らない)。
func runVerify(ctx context.Context, c registry.Command, _ []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}
	in, _ := config.Load(root)
	cfg, _ := config.NewResolver(root).Resolve(in)
	st, _ := state.Load(root, cfg.Project)

	var outcomes []verifyOutcome
	if rec, ok := publishedRecord(st, "homebrew"); ok {
		oc, err := verifyHomebrew(ctx, cfg, rec)
		if err != nil {
			return internalError(c, err)
		}
		outcomes = append(outcomes, oc)
	}
	for _, name := range []string{"apt", "rpm"} {
		if rec, ok := publishedRecord(st, name); ok {
			outcomes = append(outcomes, verifyLinuxRepo(ctx, name, cfg, in, rec))
		}
	}

	if len(outcomes) == 0 {
		res := output.New(c.Name, "nothing published to verify yet", true)
		res.Next = []output.NextDo{{Reason: "publish first, then verify the install", Do: "wharfy publish homebrew --yes"}}
		return res
	}
	return verifyResult(c, outcomes)
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
// 1 つでも failed なら ok=false。skipped は失敗ではないので ok を落とさない。
func verifyResult(c registry.Command, outcomes []verifyOutcome) output.Result {
	var checks []verifyCheck
	var problems []output.Problem
	var warnings []output.Warning
	var nexts []output.NextDo
	failed := false
	for _, oc := range outcomes {
		checks = append(checks, oc.check)
		if oc.check.Status == verifyStatusFailed {
			failed = true
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

	res := output.New(c.Name, verifyMessage(checks), !failed)
	res.Data = verifyData{Checks: checks}
	res.Warnings = warnings
	res.Errors = problems
	if failed {
		res.Next = nexts
	} else {
		res.Next = []output.NextDo{{Reason: "distribution looks consistent; review overall state", Do: "wharfy status"}}
	}
	return res
}

// verifyMessage は「何が通り、何が落ち、何を飛ばしたか」を一行にする。
func verifyMessage(checks []verifyCheck) string {
	byStatus := map[string][]string{}
	for _, ck := range checks {
		byStatus[ck.Status] = append(byStatus[ck.Status], ck.Channel)
	}
	var parts []string
	for _, st := range []string{verifyStatusFailed, verifyStatusOK, verifyStatusSkipped} {
		if names := byStatus[st]; len(names) > 0 {
			parts = append(parts, st+" "+strings.Join(names, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

// verifyHomebrew は自前 tap の formula が在り、版が記録と一致するかを照合する。
// 記録された tap が解決できないのは設定の壊れ(検証の失敗ではない)なので error で返す。
func verifyHomebrew(ctx context.Context, cfg config.Config, rec state.PublishRecord) (verifyOutcome, error) {
	tap := firstNonEmptyStr(rec.Target, homebrewTargetOrEmpty(cfg))
	owner, repo, ok := splitOwnerName(tap)
	if !ok {
		return verifyOutcome{}, errString("recorded homebrew target is unresolved: " + tap)
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
func verifyLinuxRepo(ctx context.Context, name string, cfg config.Config, in config.File, rec state.PublishRecord) verifyOutcome {
	repo := firstNonEmptyStr(rec.Target, channelTarget(cfg, name))
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
		return verifySkip(name, name+" "+rs.Version+" found in "+repo+", but the install was not exercised: docker is not available")
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

// verifySuccess / verifyFailure / verifySkip / probeFailedOutcome は 1 チャネル分の結果を組む。
func verifySuccess(name, msg string) verifyOutcome {
	return verifyOutcome{check: verifyCheck{Channel: name, Status: verifyStatusOK, Message: msg}}
}

func verifyFailure(name, msg, problem, hint, detail, next string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusFailed, Message: msg},
		problem: &output.Problem{Code: output.ErrVerifyFailed, Message: problem, Hint: hint, Detail: detail},
		next:    &output.NextDo{Reason: "re-publish " + name + " to fix what verify found", Do: next},
	}
}

// verifySkip は検証を飛ばした事実を warning として残す。ok は落とさないが、黙って通してもいない。
func verifySkip(name, msg string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: name, Status: verifyStatusSkipped, Message: msg},
		warning: &output.Warning{Code: output.WarnChannelSkipped, Message: msg},
	}
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

// channelTarget は解決済みチャネルの配信先(無ければ空)。
func channelTarget(cfg config.Config, name string) string {
	for _, ch := range cfg.Channels {
		if ch.Name == name {
			return ch.Target
		}
	}
	return ""
}

// homebrewTargetOrEmpty は cfg の homebrew tap(無ければ空)。
func homebrewTargetOrEmpty(cfg config.Config) string {
	t, _ := homebrewTarget(cfg)
	return t
}

type errString string

func (e errString) Error() string { return string(e) }
