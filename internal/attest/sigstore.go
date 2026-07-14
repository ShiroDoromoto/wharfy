package attest

// sigstore.go — 証言に keyless 署名して Sigstore bundle にする(sigstore-go・D-248)。
//
// 鍵は持たない。その場限りの鍵を作り、Actions が配る OIDC の身分で Fulcio に短命証明書を切らせ、
// 署名し、透明性ログ(Rekor)に載せる。鍵の保管も失効も要らないのが keyless の意味で、
// 「誰が・どの workflow から作ったか」は証明書の中に GitHub が刻む。
//
// 信頼の錨(どの Fulcio / Rekor が正なのか)は TUF から取る。ここを定数で焼くと、公開 Sigstore が
// 鍵を回した日に黙って壊れる。

import (
	"context"
	"fmt"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"google.golang.org/protobuf/encoding/protojson"
)

// SigstoreSigner は公開 Sigstore(public-good instance)で署名する Signer。
type SigstoreSigner struct{}

// NewSigner は既定の Signer を返す(生成点——テストはここを差し替える)。
func NewSigner() Signer { return SigstoreSigner{} }

// rekorV1Only は透明性ログの選好。公開 Sigstore は Rekor v2 も配り始めているが、証明を**検算する**
// 側(gh attestation verify 等)が v1 しか読めない場合、v2 のエントリを載せた bundle は
// 「署名は正しいのに誰も検算できない」証明になる。v1 を先に選び、v1 が無い場合だけライブラリの
// 対応版に委ねる。
var rekorV1Only = []uint32{1}

// SignDSSE は in-toto の証言を DSSE として署名し、Sigstore bundle(JSON)を返す。
func (SigstoreSigner) SignDSSE(ctx context.Context, statement []byte, idToken string) ([]byte, error) {
	tufClient, err := tuf.New(tuf.DefaultOptions())
	if err != nil {
		return nil, fmt.Errorf("attest: sigstore trust root: %w", err)
	}
	trustedRoot, err := root.GetTrustedRoot(tufClient)
	if err != nil {
		return nil, fmt.Errorf("attest: sigstore trusted root: %w", err)
	}
	signingConfig, err := root.GetSigningConfig(tufClient)
	if err != nil {
		return nil, fmt.Errorf("attest: sigstore signing config: %w", err)
	}
	keypair, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return nil, fmt.Errorf("attest: ephemeral keypair: %w", err)
	}

	now := time.Now()
	opts := sign.BundleOptions{Context: ctx, TrustedRoot: trustedRoot}

	fulcio, err := root.SelectService(signingConfig.FulcioCertificateAuthorityURLs(), sign.FulcioAPIVersions, now)
	if err != nil {
		return nil, fmt.Errorf("attest: select fulcio: %w", err)
	}
	opts.CertificateProvider = sign.NewFulcio(&sign.FulcioOptions{BaseURL: fulcio.URL, Timeout: 30 * time.Second, Retries: 1})
	opts.CertificateProviderOptions = &sign.CertificateProviderOptions{IDToken: idToken}

	logs, err := root.SelectServices(signingConfig.RekorLogURLs(), signingConfig.RekorLogURLsConfig(), rekorV1Only, now)
	if err != nil {
		logs, err = root.SelectServices(signingConfig.RekorLogURLs(), signingConfig.RekorLogURLsConfig(), sign.RekorAPIVersions, now)
		if err != nil {
			return nil, fmt.Errorf("attest: select rekor: %w", err)
		}
	}
	for _, l := range logs {
		opts.TransparencyLogs = append(opts.TransparencyLogs, sign.NewRekor(&sign.RekorOptions{
			BaseURL: l.URL, Timeout: 90 * time.Second, Retries: 1, Version: l.MajorAPIVersion,
		}))
	}

	// 署名時刻の証明。透明性ログのエントリが既に時刻を担うので、TSA が引けない構成でも証明は成立する
	// ——ここで落とすと「あれば良かった」ものの不在で来歴ごと失う。
	if tsas, err := root.SelectServices(signingConfig.TimestampAuthorityURLs(), signingConfig.TimestampAuthorityURLsConfig(), sign.TimestampAuthorityAPIVersions, now); err == nil {
		for _, t := range tsas {
			opts.TimestampAuthorities = append(opts.TimestampAuthorities, sign.NewTimestampAuthority(&sign.TimestampAuthorityOptions{
				URL: t.URL, Timeout: 30 * time.Second, Retries: 1,
			}))
		}
	}

	bundle, err := sign.Bundle(&sign.DSSEData{Data: statement, PayloadType: PayloadType}, keypair, opts)
	if err != nil {
		return nil, fmt.Errorf("attest: sign: %w", err)
	}
	b, err := protojson.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("attest: marshal bundle: %w", err)
	}
	return b, nil
}
