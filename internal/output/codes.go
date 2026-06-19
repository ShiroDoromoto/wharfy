package output

// codes.go — warning / error コードの正準カタログ(設計 09)。
//
// 規約(09):
//   - コードは snake_case の安定識別子。**追加は非破壊**、**改名・意味変更は破壊的**
//     (02 の schema_version と同じ扱い)。
//   - message は人間向け(変わってよい)。**分岐は code で行う**(message でしない)。
//   - 新しい失敗パターンは、まずここに code を足してから実装する(後付けの無秩序な文字列を作らない)。
//
// Result を作る箇所はここの定数だけを使う。Catalog がこのパッケージの単一真実で、
// doc/wip/wharfy/design/09_error_catalog.md(人間向けカタログ)との一致を codes_test.go で担保する。

// 警告コード(warnings・処理は続行)。
const (
	WarnWinUnsigned       = "win_unsigned"        // Windows 成果物が未署名(証明書なし)
	WarnDarwinUnnotarized = "darwin_unnotarized"  // darwin 署名済みだが未公証
	WarnChannelSkipped    = "channel_skipped"     // チャネルを skip(トークン/設定不足)
	WarnDriftDetected     = "drift_detected"      // status で記録と実体が食い違い(04)
	WarnGatedPending      = "gated_pending"       // gated チャネルが審査待ち
	WarnGoinstallOnlyGo   = "goinstall_only_go"   // goinstall 指定だが Go ターゲットでない
	WarnTapWillBeCreated  = "tap_will_be_created" // 自前 tap/bucket が未作成で作る予定
)

// エラーコード(errors・ok=false で停止)。
const (
	ErrConfigInvalid      = "config_invalid"       // wharfy.yaml が不正(スキーマ違反)
	ErrMainAmbiguous      = "main_ambiguous"       // main を推測できない(複数 main)
	ErrGithubUnresolved   = "github_unresolved"    // github を推測できない(remote 不在等)
	ErrTagMissing         = "tag_missing"          // tag 上でない/tag が無い
	ErrBuildFailed        = "build_failed"         // クロスビルド失敗
	ErrBuilderUnavailable = "builder_unavailable"  // 下層ビルダ(GoReleaser)が見つからない/起動不可
	ErrTokenMissing       = "token_missing"        // その操作に必須のトークン未設定
	ErrAuthFailed         = "auth_failed"          // トークン/鍵はあるが認証失敗
	ErrKeychainFailed     = "keychain_failed"      // OS keychain への保存/読み出しに失敗(ロック/権限)
	ErrTargetCreateFailed = "target_create_failed" // 自前 tap/bucket/repo 作成失敗
	ErrConsentRequired    = "consent_required"     // strict gated への申請に明示同意が必要(未同意)
	ErrPublishFailed      = "publish_failed"       // チャネルへの発行失敗
	ErrProbeFailed        = "probe_failed"         // 実体照合に失敗(04)
	ErrNetworkError       = "network_error"        // 一時的なネットワーク失敗
	ErrVerifyFailed       = "verify_failed"        // verify で install/実行が失敗
	ErrInternal           = "internal"             // 想定外(バグ)
)

// CodeKind は正準カタログ内での分類。warning=処理続行 / error=ok=false で停止。
type CodeKind string

const (
	KindWarning CodeKind = "warning"
	KindError   CodeKind = "error"
)

// CatalogEntry は正準カタログの 1 行。Summary は「いつ起きるか」(09 の表の説明)。
type CatalogEntry struct {
	Code    string
	Kind    CodeKind
	Summary string
}

// Catalog は 09 の正準リストをコードに写したもの。コード定数と 1:1 で対応する。
// 並びは 09 の表と同順。
var Catalog = []CatalogEntry{
	{WarnWinUnsigned, KindWarning, "Windows 成果物が未署名(証明書なし)"},
	{WarnDarwinUnnotarized, KindWarning, "darwin 署名済みだが未公証"},
	{WarnChannelSkipped, KindWarning, "チャネルを skip(トークン/設定不足)"},
	{WarnDriftDetected, KindWarning, "status で記録と実体が食い違い"},
	{WarnGatedPending, KindWarning, "gated チャネルが審査待ち"},
	{WarnGoinstallOnlyGo, KindWarning, "goinstall 指定だが Go ターゲットでない"},
	{WarnTapWillBeCreated, KindWarning, "自前 tap/bucket が未作成で作る予定"},

	{ErrConfigInvalid, KindError, "wharfy.yaml が不正(スキーマ違反)"},
	{ErrMainAmbiguous, KindError, "main を推測できない(複数 main)"},
	{ErrGithubUnresolved, KindError, "github を推測できない(remote 不在等)"},
	{ErrTagMissing, KindError, "tag 上でない/tag が無い"},
	{ErrBuildFailed, KindError, "クロスビルド失敗"},
	{ErrBuilderUnavailable, KindError, "下層ビルダが見つからない/起動不可"},
	{ErrTokenMissing, KindError, "その操作に必須のトークン未設定"},
	{ErrAuthFailed, KindError, "トークン/鍵はあるが認証失敗"},
	{ErrKeychainFailed, KindError, "OS keychain への保存/読み出しに失敗(ロック/権限)"},
	{ErrTargetCreateFailed, KindError, "自前 tap/bucket/repo 作成失敗"},
	{ErrConsentRequired, KindError, "strict gated への申請に明示同意が必要(未同意)"},
	{ErrPublishFailed, KindError, "チャネルへの発行失敗"},
	{ErrProbeFailed, KindError, "実体照合に失敗"},
	{ErrNetworkError, KindError, "一時的なネットワーク失敗"},
	{ErrVerifyFailed, KindError, "verify で install/実行が失敗"},
	{ErrInternal, KindError, "想定外(バグ)"},
}

// KnownCode は code が正準カタログに存在するかを返す。Result 組み立て時の自己点検に使う。
func KnownCode(code string) bool {
	for _, e := range Catalog {
		if e.Code == code {
			return true
		}
	}
	return false
}

// catalogCodes は Catalog の code 集合(テスト・補完生成などの照合用)。
func catalogCodes() map[string]CodeKind {
	m := make(map[string]CodeKind, len(Catalog))
	for _, e := range Catalog {
		m[e.Code] = e.Kind
	}
	return m
}
