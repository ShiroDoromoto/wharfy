package attest

// oidc.go — keyless 署名の身分証(OIDC トークン)を Actions から取る。
//
// Fulcio は「鍵」ではなく「身分」で証明書を切る。その身分を配るのが Actions で、workflow に
// permissions: id-token: write があるとき **だけ** 取り口(URL とその token)を env に置く。
// wharfy が資格情報を env からしか取らない作法と、そのまま噛み合う。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// OIDCEnv は Actions が置く OIDC の取り口。
type OIDCEnv struct {
	RequestURL   string // ACTIONS_ID_TOKEN_REQUEST_URL
	RequestToken string // ACTIONS_ID_TOKEN_REQUEST_TOKEN
}

// Available は OIDC を取れるか(＝ここが CI で、id-token: write が与えられているか)。
func (e OIDCEnv) Available() bool { return e.RequestURL != "" && e.RequestToken != "" }

// ActionsTokens は Actions の取り口から OIDC トークンを取る TokenSource。
type ActionsTokens struct {
	Env  OIDCEnv
	HTTP *http.Client
}

var errNoOIDC = errors.New("attest: no OIDC token endpoint in the environment (this is not GitHub Actions, or the workflow lacks permissions: id-token: write)")

// IDToken は audience 宛ての OIDC トークンを取る。audience が違うトークンは Fulcio が受け取らない。
func (a ActionsTokens) IDToken(ctx context.Context, audience string) (string, error) {
	if !a.Env.Available() {
		return "", errNoOIDC
	}
	u, err := url.Parse(a.Env.RequestURL)
	if err != nil {
		return "", fmt.Errorf("attest: bad OIDC request url: %w", err)
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.Env.RequestToken)
	req.Header.Set("Accept", "application/json")

	hc := a.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("attest: request OIDC token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("attest: request OIDC token: %s", resp.Status)
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("attest: decode OIDC token: %w", err)
	}
	if body.Value == "" {
		return "", errors.New("attest: OIDC endpoint returned an empty token")
	}
	return body.Value, nil
}
