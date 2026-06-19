// Package secret は配布トークンを OS keychain に保管する薄いラッパー
// (macOS Keychain / Windows Credential Manager / Linux Secret Service を go-keyring で横断)。
//
// wharfy はトークンをどこへも送らない。これは利用者自身の配布先トークンを利用者の手元に置くだけ。
// 値は hidden prompt(端末)で入力され、agent のコンテキスト(会話)を通らないのが要点
// (crofty internal/secret と同じ原則)。CI 等は環境変数で渡せるので、keychain は任意の補助。
//
// 依存方向: ドメイン層なので上位(output/emit・CLI)を import しない。
package secret

import (
	"errors"

	"github.com/zalando/go-keyring"
)

// service は wharfy の keychain エントリをまとめる名前(アンインストール時の整理用)。
const service = "wharfy"

// ErrNotFound は name に対応する値が無いとき返る(呼び出し側で env フォールバック等に使う)。
var ErrNotFound = errors.New("secret not found")

// Get は保存済みの値を返す。無ければ ErrNotFound。
func Get(name string) (string, error) {
	v, err := keyring.Get(service, name)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return v, err
}

// Set は値を保存(既存は置換)する。
func Set(name, value string) error {
	return keyring.Set(service, name, value)
}

// Delete は値を削除する。無いのはエラーにしない。
func Delete(name string) error {
	if err := keyring.Delete(service, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}
