package channel

import "strings"

// diff.go — 書き込み前に見せる行差分(設計 03「ブラックボックスにしない」)。
// 所有配布物の旧 vs 新を unified 風に出す。依存を増やさず LCS で最小実装。

// Diff は old→new の行差分を "+/-/ " 行で返す。old が空なら全行を追加として返す。
func Diff(old, new string) string {
	if old == new {
		return ""
	}
	oldLines := splitLines(old)
	newLines := splitLines(new)

	lcs := lcsTable(oldLines, newLines)
	var b strings.Builder
	i, j := 0, 0
	emit := func(prefix, line string) { b.WriteString(prefix); b.WriteString(line); b.WriteByte('\n') }
	for i < len(oldLines) && j < len(newLines) {
		if oldLines[i] == newLines[j] {
			emit(" ", oldLines[i])
			i++
			j++
		} else if lcs[i+1][j] >= lcs[i][j+1] {
			emit("-", oldLines[i])
			i++
		} else {
			emit("+", newLines[j])
			j++
		}
	}
	for ; i < len(oldLines); i++ {
		emit("-", oldLines[i])
	}
	for ; j < len(newLines); j++ {
		emit("+", newLines[j])
	}
	return b.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// lcsTable は最長共通部分列の長さ表(末尾から)。lcs[i][j] = old[i:] と new[j:] の LCS 長。
func lcsTable(a, b []string) [][]int {
	t := make([][]int, len(a)+1)
	for i := range t {
		t[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else if t[i+1][j] >= t[i][j+1] {
				t[i][j] = t[i+1][j]
			} else {
				t[i][j] = t[i][j+1]
			}
		}
	}
	return t
}
