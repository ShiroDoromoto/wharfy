package state

import (
	"strconv"
	"strings"
)

// reconcile.go — ハイブリッド状態の照合(設計 04)。記録(recorded)と実体(remote)を突き合わせ、
// source と drift を決める。drift は黙って合わせず status に見せる(③)。

// source の値(common.json stateSource)。
const (
	SourceRecorded = "recorded" // ローカル記録のみ(未照合)
	SourceProbed   = "probed"   // 実体照合済み・一致
	SourceDrift    = "drift"    // 食い違い検出
)

// drift の種別(common.json driftKind)。
const (
	DriftBehind    = "behind"    // 実体が記録より古い(発行が反映前/失敗)
	DriftAhead     = "ahead"     // 実体が記録より新しい(外部で更新)
	DriftMissing   = "missing"   // 実体に無い(消えた/未到達)
	DriftUntracked = "untracked" // 記録に無い(wharfy 外で発行)
)

// Drift は記録と実体の食い違いの詳細(common.json drift)。一致時は nil。
type Drift struct {
	Recorded string `json:"recorded,omitempty"`
	Remote   string `json:"remote,omitempty"`
	Kind     string `json:"kind"`
}

// Reconcile は記録版 recorded と実体版 remote(remoteFound=実体に在るか)から
// source と drift を決める(04 の照合表)。probed=false なら未照合＝recorded で返す。
func Reconcile(recorded, remote string, remoteFound, probed bool) (source string, drift *Drift) {
	if !probed {
		return SourceRecorded, nil
	}
	switch {
	case recorded == "" && !remoteFound:
		return SourceProbed, nil // 記録も実体も無い(未発行)
	case recorded == "" && remoteFound:
		return SourceDrift, &Drift{Remote: remote, Kind: DriftUntracked}
	case recorded != "" && !remoteFound:
		return SourceDrift, &Drift{Recorded: recorded, Kind: DriftMissing}
	}
	switch compareVersions(remote, recorded) {
	case 0:
		return SourceProbed, nil
	case -1:
		return SourceDrift, &Drift{Recorded: recorded, Remote: remote, Kind: DriftBehind}
	default:
		return SourceDrift, &Drift{Recorded: recorded, Remote: remote, Kind: DriftAhead}
	}
}

// compareVersions は先頭 v を無視し、ドット区切りの数値部を順に比べる。
// 数値化できない部分は文字列比較にフォールバックする。返り値 -1/0/1。
func compareVersions(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		sa, sb := part(pa, i), part(pb, i)
		na, ea := strconv.Atoi(sa)
		nb, eb := strconv.Atoi(sb)
		if ea == nil && eb == nil {
			if na != nb {
				return sign(na - nb)
			}
			continue
		}
		if sa != sb {
			return strings.Compare(sa, sb)
		}
	}
	return 0
}

func part(p []string, i int) string {
	if i < len(p) {
		return p[i]
	}
	return "0"
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}
