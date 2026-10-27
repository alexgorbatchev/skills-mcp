package authoring

import (
	"fmt"
	"regexp"
)

// denyScan returns one entry per deny-pattern hit in added lines, file
// paths, or the commit message.
func denyScan(patterns []*regexp.Regexp, lines, paths []string, message string) []string {
	var hits []string
	check := func(kind, text string) {
		for _, re := range patterns {
			if re.MatchString(text) {
				hits = append(hits, fmt.Sprintf("%s matches %s", kind, re))
			}
		}
	}
	for _, l := range lines {
		check("an added line", l)
	}
	for _, p := range paths {
		check("path "+p, p)
	}
	check("the commit message", message)
	return hits
}
