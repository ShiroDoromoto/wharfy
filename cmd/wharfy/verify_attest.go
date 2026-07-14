package main

// verify_attest.go — 配ってある物の来歴を、消費者の目で検算する(verify の attest 行)。
//
// release は証明を「付けた」で終わる。付いているかを確かめられるのは受け取る側だけで、そこを見ない限り
// 「証明したつもり」は緑のまま出ていく——署名の形が違う・透明性ログに載っていない・想定と別の workflow が
// 署名した・一部の成果物にしか付いていない。どれも配る側からは見えない。
//
// 引き当てる digest は GitHub が資産のバイト列から出したもの(ReleaseAudit.Digests)。配布者が書いた
// checksums マニフェストではなく**いま落ちてくる物**の digest なので、資産を落とさずに(D-4)、利用者が
// 受け取るバイト列そのものの来歴を引ける。container の image も同じ作法で、レジストリがいま返す
// manifest digest で引く(publish が push 後に付けた証明が、そこに在るはず)。
//
// 来歴は digest で引くので、tap も bucket も apt/rpm も、同じバイト列を配っている限りこの証明が
// そのまま当たる——チャネルごとに行を立てないのはそのため(status の attest.covered が同じことを言う)。

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/ShiroDoromoto/wharfy/internal/attest"
	"github.com/ShiroDoromoto/wharfy/internal/channel"
	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
)

// attestCheckName は verify の checks に載る行の名前。チャネルではない(配布物の来歴は
// チャネル横断の性質)が、確かめた事実は 1 行 1 事実で並べる方が読める。
const attestCheckName = "attest"

// 生成点(テストで差し替える——ここから先はネットワークと Sigstore の信頼の錨を踏む)。
var (
	newAttestFetcher = func(owner, repo, token string) attest.Fetcher {
		return attest.NewGitHubStore(owner, repo, token)
	}
	newAttestVerifier = func() attest.BundleVerifier { return attest.NewVerifier() }
)

// verifyAttest は配ってある物(Release の資産と container の image)に付いた来歴を検算する。
//
// 段階は 3 つ。どれも「証明が無い」と「証明が壊れている」を混ぜない:
//   - 1 つも付いていない → partial(まだ付けていない配布。CI の permissions を足せば付く)
//   - 一部にしか付いていない → failed(付けたつもりの取りこぼし——これを捕まえるのがこの行の主目的)
//   - 付いているのに検算できない → failed(誰も検算できない証明は、無いのと同じか、それ以下)
func verifyAttest(ctx context.Context, cfg config.Config, ch config.ResolvedChannel, tgt verifyTarget, audit channel.ReleaseAudit) verifyOutcome {
	repo := firstNonEmptyStr(ch.Target, tgt.Target)
	subjects := attestSubjectsOnRelease(audit)
	if img, ok := attestSubjectOnImage(ctx, cfg, tgt.Version); ok {
		subjects = append(subjects, img)
	}
	if len(subjects) == 0 {
		// digest が引けない(古い GitHub が digest を返さない)か、成果物が 1 つも無い。どちらも
		// 「来歴を確かめられなかった」——確かめていないことを緑で返さない。
		return verifyNotRun(attestCheckName,
			attestCheckName+" skipped: the release carries no asset whose digest github reports, so there is nothing to look provenance up by")
	}

	owner, name, ok := splitOwnerName(repo)
	if !ok {
		return verifySkip(attestCheckName, attestCheckName+": cannot resolve the github owner/repo the attestations are stored on")
	}
	cov, err := attest.Verify(ctx, subjects, attest.ActionsIdentity(repo),
		newAttestFetcher(owner, name, os.Getenv("GITHUB_TOKEN")), newAttestVerifier())
	if err != nil {
		return probeFailedOutcome(attestCheckName, err)
	}

	total := strconv.Itoa(len(subjects))
	missing, broken := cov.Missing(), cov.Broken()
	switch {
	case len(broken) > 0:
		return attestFailure(
			attestCheckName+": "+strconv.Itoa(len(broken))+" of "+total+" shipped artifacts carry provenance that does not verify",
			"the build provenance stored for these artifacts cannot be verified by a consumer",
			"`gh attestation verify <file> --repo "+repo+"` fails for anyone checking them — re-run release on this tag to attest them again",
			attestDetail(broken))
	case len(missing) == len(subjects):
		msg := attestCheckName + ": none of the " + total + " shipped artifacts on v" + tgt.Version +
			" carry build provenance — nothing proves they came from this repository's workflow"
		return verifyOutcome{
			check: verifyCheck{Channel: attestCheckName, Status: verifyStatusPartial, Message: msg},
			warning: &output.Warning{
				Code: output.WarnAttestUnavailable,
				Message: msg + "; release attaches it when it runs in github actions with permissions: " +
					strings.Join(registry.AttestPermissions, " / "),
			},
		}
	case len(missing) > 0:
		return attestFailure(
			attestCheckName+": "+strconv.Itoa(len(missing))+" of "+total+" shipped artifacts carry no build provenance",
			"the release is only partly attested, so whether a user can prove where their download came from depends on which artifact they took",
			"release attests everything it uploads and publish attests the image it pushes, so a gap means these never reached the attest step — re-run release/publish on this tag",
			attestDetail(missing))
	}
	return verifySuccess(attestCheckName,
		attestCheckName+": all "+total+" shipped artifacts on v"+tgt.Version+
			" carry verifiable provenance from this repository's workflow (signed, logged in rekor, and bound to the bytes users receive)")
}

// attestFailure は来歴が検算できないことを failed として組む。
//
// verifyFailure と別に持つのは次の一手が違うから: チャネルの失敗は「そのチャネルへ publish し直す」だが、
// 来歴を付け直せるのは release だけ(証明は成果物のバイト列に結びついている)。
func attestFailure(msg, problem, hint, detail string) verifyOutcome {
	return verifyOutcome{
		check:   verifyCheck{Channel: attestCheckName, Status: verifyStatusFailed, Message: msg},
		problem: &output.Problem{Code: output.ErrAttestUnverified, Message: problem, Hint: hint, Detail: detail},
		next:    &output.NextDo{Reason: "re-run release on this tag to attest the artifacts again", Do: "wharfy release --yes"},
	}
}

// attestSubjectsOnRelease は Release 資産のうち、release が来歴を付けた対象を返す。
//
// 期待集合を verify の側で組み直すのが要点: release が「上げた物」ではなく Release に「在る物」から
// 数えるので、attest 段が subject を取りこぼしていれば、その資産が missing として現れる。
func attestSubjectsOnRelease(audit channel.ReleaseAudit) []attest.Subject {
	var subs []attest.Subject
	for _, name := range sortedKeys(audit.Digests) {
		if !isAttestedAsset(name) {
			continue
		}
		subs = append(subs, attest.Subject{Name: name, SHA256: audit.Digests[name]})
	}
	return subs
}

// attestSubjectOnImage は container が配っている image の manifest digest を subject にする。
//
// ここも「在る物」から数える: レジストリがいま返す digest で引くので、publish が push だけして証明を
// 付け損ねていれば、その image が missing として現れる(image に来歴が付いた今、それを見ないと
// 「付けたつもり」がまた 1 つ増える)。
//
// 引けないなら subject にしない —— container を配っていない・tag がまだ無い・digest を返さない
// レジストリ。image の不在は container の行が言うことで、attest の行が二重に言うことではない。
func attestSubjectOnImage(ctx context.Context, cfg config.Config, version string) (attest.Subject, bool) {
	if !config.HasChannel(cfg, "container") {
		return attest.Subject{}, false
	}
	image := channelTargetByName(cfg, "container")
	if image == "" {
		return attest.Subject{}, false
	}
	sub, found, err := imageSubject(ctx, image, version)
	if err != nil || !found || sub.SHA256 == "" {
		return attest.Subject{}, false
	}
	return sub, true
}

// isAttestedAsset は Release 資産に来歴が付いているはずか(＝ attest 段が subject にする物か)。
//
// release は**上げた物**を等しく証明する ——アーカイブもパッケージもバンドルも、install.sh /
// install.ps1 / latest.json も。唯一の例外が checksums マニフェストで、あれは資産ではなく資産の
// 記述(goreleaser が作り、goreleaser が上げる)。そこに載っている資産は 1 つずつ証明されている。
func isAttestedAsset(name string) bool {
	return !channel.IsChecksumsManifest(name)
}

// attestDetail は落ちた subject を 1 行 1 件で述べる(資産名と、引き当てに使った digest)。
func attestDetail(cks []attest.Checked) string {
	lines := make([]string, 0, len(cks))
	for _, ck := range cks {
		line := ck.Subject.Name + " (sha256:" + ck.Subject.SHA256 + ")"
		if ck.Err != nil {
			line += ": " + ck.Err.Error()
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
