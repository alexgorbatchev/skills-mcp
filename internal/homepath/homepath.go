// Package homepath folds the user's home directory to "~" in printed paths.
package homepath

import (
	"os"
	"path/filepath"
	"strings"
)

// Fold returns p with a leading home directory shown as "~". Only whole path
// segments match: with home /home/dev, /home/developer is unchanged.
func Fold(p string) string {
	return foldWith(home(), p)
}

// FoldText folds every occurrence of the home directory that forms a whole
// path prefix inside free text, such as an error message.
func FoldText(s string) string {
	h := home()
	if h == "" || h == string(filepath.Separator) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, h)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(h)
		atBoundary := end == len(s) || s[end] == filepath.Separator || strings.ContainsRune(" \t\n\"':,;)]}", rune(s[end]))
		startOK := i == 0 || strings.ContainsRune(" \t\n\"'=(:[{", rune(s[i-1]))
		b.WriteString(s[:i])
		if atBoundary && startOK {
			b.WriteString("~")
		} else {
			b.WriteString(h)
		}
		s = s[end:]
	}
}

func foldWith(h, p string) string {
	if h == "" || h == string(filepath.Separator) {
		return p
	}
	if p == h {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, h+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return p
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Clean(h)
}
