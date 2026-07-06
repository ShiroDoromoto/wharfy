package main

import (
	"context"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ShiroDoromoto/wharfy/internal/config"
	"github.com/ShiroDoromoto/wharfy/internal/output"
	"github.com/ShiroDoromoto/wharfy/internal/registry"
	"github.com/ShiroDoromoto/wharfy/internal/sign"
)

// runSign は署名段(依頼①)。identity が解決でき、prebuilt に darwin があれば **実際に署名**して
// 結果を報告する。identity 未指定 / 非 prebuilt / darwin 非対象では従来どおり素通し(advisory)で、
// no-op を「署名した」と偽装しない。未署名はブロックしない(署名は要件でなく品質)。
func runSign(ctx context.Context, c registry.Command, _ []string) output.Result {
	root, err := os.Getwd()
	if err != nil {
		return internalError(c, err)
	}
	in, _ := config.Load(root)
	cfg, _ := config.NewResolver(root).Resolve(in) // main 曖昧でも sign は出せる(ビルドしない)

	goos := config.DefaultGOOS
	if cfg.Build != nil && len(cfg.Build.GOOS) > 0 {
		goos = cfg.Build.GOOS
	}
	opts := resolveSignOptions(in)

	// P12 を渡したのに identity が無いと codesign は誰の名で署名するか決められない(誤設定を明示)。
	if opts.P12 != "" && opts.Identity == "" {
		res := output.New(c.Name, "signing: p12 provided but no identity — set sign.identity (the certificate common name)", true)
		res.Data = map[string]any{"sign": sign.Status(goos, opts)}
		res.Warnings = append(res.Warnings, output.Warning{
			Code:    output.WarnWinUnsigned,
			Message: "p12 set without sign.identity — codesign needs the certificate name to sign",
		})
		res.Next = []output.NextDo{{Reason: "name the certificate to sign with", Do: "set sign.identity in wharfy.yaml (or WHARFY_SIGN_IDENTITY)"}}
		return res
	}

	// 実署名の条件: prebuilt(BYO-binary)で identity があり、darwin バイナリを持ち込んでいる。
	if cfg.Prebuilt && opts.Enabled() {
		bins := toPrebuiltBinaries(in)
		if hasDarwin(bins) {
			_, signed, serr := stageSignDarwin(ctx, root, config.DistDir, opts, bins)
			if serr != nil {
				return signErrorResult(c, serr)
			}
			return signSucceeded(c, opts, signed)
		}
	}

	// 素通し(advisory): 何を待っているかを status で機械可読に示す。
	status := sign.Status(goos, opts)
	res := output.New(c.Name, signMessage(status, opts), true)
	res.Data = map[string]any{"sign": status}
	if t, ok := status["windows"]; ok && !t.Signed {
		res.Warnings = append(res.Warnings, output.Warning{
			Code:    output.WarnWinUnsigned,
			Message: "windows unsigned — no certificate configured",
		})
	}
	// 未署名は warning のまま publish 可能(no-op の export 偽装をしない)。
	res.Next = []output.NextDo{
		{Reason: "unsigned is a warning, not a blocker — continue to publish", Do: "wharfy publish homebrew"},
	}
	return res
}

// signSucceeded は実署名した結果を report する(darwin を signed で見せ、次は release へ導く)。
func signSucceeded(c registry.Command, opts sign.Options, signed []signedBinary) output.Result {
	status := map[string]sign.Target{
		"darwin": {Signed: true, Reason: "signed with " + opts.Identity},
	}
	res := output.New(c.Name, "signed darwin ("+opts.Identity+"): "+pluralBinaries(len(signed))+" — checksums finalize at `wharfy release`", true)
	res.Data = map[string]any{"sign": status, "signed": signed}
	res.Next = []output.NextDo{
		{Reason: "sign is done; release cuts the signed archives and their checksums", Do: "wharfy release --yes"},
	}
	return res
}

// signErrorResult は署名失敗(codesign 不在=誤設定 / codesign 失敗)を明示する。
// identity を指定したのに署名できない状態を素通しで隠さず fail する(未署名を「署名済み」と誤認させない)。
func signErrorResult(c registry.Command, err error) output.Result {
	var unavail *sign.UnavailableError
	hint := "check the signing identity and that the binaries exist"
	if errors.As(err, &unavail) {
		hint = "codesign is macOS-only — run sign/release on macOS, or drop sign.identity to bring your own pre-signed binaries"
	}
	res := output.New(c.Name, "sign failed: "+err.Error(), false)
	res.Errors = []output.Problem{{Code: output.ErrSignFailed, Message: err.Error(), Hint: hint}}
	res.Next = []output.NextDo{{Reason: "fix signing then retry", Do: "wharfy sign"}}
	return res
}

// signMessage は素通し時の状態を一行に畳む。identity 設定の有無で advisory の理由が変わる。
func signMessage(status map[string]sign.Target, opts sign.Options) string {
	oses := make([]string, 0, len(status))
	for os := range status {
		oses = append(oses, os)
	}
	sort.Strings(oses)
	parts := make([]string, 0, len(oses))
	for _, os := range oses {
		t := status[os]
		switch {
		case t.Signed && t.Notarized:
			parts = append(parts, os+" signed+notarized")
		case t.Signed:
			parts = append(parts, os+" signed")
		default:
			parts = append(parts, os+" unsigned")
		}
	}
	if len(parts) == 0 {
		return "signing: nothing to sign (advisory; bring your own pre-signed binaries or set sign.identity)"
	}
	if opts.Enabled() {
		return "signing (identity set; run `wharfy release` to sign prebuilt darwin binaries): " + strings.Join(parts, ", ")
	}
	return "signing (advisory; no identity configured — pre-signed binaries respected): " + strings.Join(parts, ", ")
}

func pluralBinaries(n int) string {
	if n == 1 {
		return "1 binary"
	}
	return strconv.Itoa(n) + " binaries"
}
