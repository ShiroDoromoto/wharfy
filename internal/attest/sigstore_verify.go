package attest

// sigstore_verify.go — 預けた bundle を sigstore-go で検算する(署名する側と同じライブラリ・D-248)。
//
// `gh attestation verify` がやることを自前でやる。外部 CLI に預けないのは署名側と同じ理由で、
// 「壊れていても緑に見える」領域を他人のフラグ仕様に委ねないため。
//
// 信頼の錨(どの Fulcio / Rekor を正とするか)は署名時と同じく TUF から取る。ここを定数で焼くと、
// 公開 Sigstore が鍵を回した日に、正しい証明を「検算できない」と言い出す。

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// SigstoreVerifier は公開 Sigstore の信頼の錨で bundle を検算する BundleVerifier。
//
// 信頼の錨の取得(TUF)はネットワークを踏むので、1 プロセス内で 1 度だけ引いて使い回す
// ——release の subject は 10 個を超える。
type SigstoreVerifier struct {
	once    sync.Once
	trusted *root.TrustedRoot
	err     error
}

// NewVerifier は既定の BundleVerifier を返す(生成点——テストはここを差し替える)。
func NewVerifier() BundleVerifier { return &SigstoreVerifier{} }

func (v *SigstoreVerifier) trustedRoot() (*root.TrustedRoot, error) {
	v.once.Do(func() {
		tufClient, err := tuf.New(tuf.DefaultOptions())
		if err != nil {
			v.err = fmt.Errorf("attest: sigstore trust root: %w", err)
			return
		}
		v.trusted, v.err = root.GetTrustedRoot(tufClient)
	})
	return v.trusted, v.err
}

// VerifyBundle は bundle を検算する。通れば nil、1 点でも欠ければその理由を返す。
//
// 要求するもの(Verify の 3 点がここで縛られる):
//   - WithTransparencyLog(1): Rekor に載っていること。載っていない証明は、後から差し替えても誰も気づけない。
//   - WithObserverTimestamps(1): 署名時刻が第三者(ログか TSA)に観測されていること——証明書の有効期間を
//     検算する基点になる(短命証明書は今はもう失効している)。
//   - WithArtifactDigest: 証言の subject が**その digest**であること。
//   - WithCertificateIdentity: 署名者が想定の repo の workflow であること。
func (v *SigstoreVerifier) VerifyBundle(ctx context.Context, bundleJSON []byte, sha256hex string, id Identity) error {
	trusted, err := v.trustedRoot()
	if err != nil {
		return err
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("attest: parse attestation bundle: %w", err)
	}
	digest, err := hex.DecodeString(sha256hex)
	if err != nil {
		return fmt.Errorf("attest: subject digest is not hex: %w", err)
	}
	certID, err := verify.NewShortCertificateIdentity(id.Issuer, "", "", id.SANRegexp)
	if err != nil {
		return fmt.Errorf("attest: certificate identity: %w", err)
	}
	verifier, err := verify.NewVerifier(trusted, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return fmt.Errorf("attest: verifier: %w", err)
	}
	_, err = verifier.Verify(&b, verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digest),
		verify.WithCertificateIdentity(certID),
	))
	return err
}
