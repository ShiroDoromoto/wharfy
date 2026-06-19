// Package sign は署名・公証の MVP(設計 10 / ADR-7: 案内のみ・状態可視化)。
//
// MVP の sign は「実行器」ではなく「状態の読み手＋案内役」。証明書取得・公証実行の
// 代行はしない(根の難しさは MVP の制御外)。正直な期待値を出力で示す。
// 将来は成果物/設定から実状態を判定する実装に差し替えるだけ(出力契約 signTarget は不変)。
package sign

// Target は OS ごとの署名/公証状態(schemas/common.json signTarget と同形)。
type Target struct {
	Signed    bool   `json:"signed"`
	Notarized bool   `json:"notarized,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Status は対象 goos の署名状態を返す。MVP は署名を構成しないため未署名で報告する
// (linux は OS レベル署名不要なのでエントリを出さない)。キーは OS 名。
func Status(goos []string) map[string]Target {
	out := map[string]Target{}
	for _, os := range goos {
		switch os {
		case "darwin":
			out["darwin"] = Target{Signed: false, Reason: "no Developer ID signing configured (MVP is advisory only)"}
		case "windows":
			out["windows"] = Target{Signed: false, Reason: "no code-signing certificate configured"}
		}
	}
	return out
}
