package catalog

import (
	"cmp"
	"slices"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

// Diff lists the tool and prompt definitions to add (or replace) and remove
// when moving from one view to the next. Only served name, description, and
// invocation make up a definition, so body and file edits produce no diff.
type Diff struct {
	AddTools      []*Skill
	RemoveTools   []string
	AddPrompts    []*Skill
	RemovePrompts []string
}

// Empty reports whether the diff changes nothing.
func (d Diff) Empty() bool {
	return len(d.AddTools)+len(d.RemoveTools)+len(d.AddPrompts)+len(d.RemovePrompts) == 0
}

// DiffViews compares two views of one harness; prev may be nil.
func DiffViews(prev, next *View) Diff {
	before := definitions(prev)
	after := definitions(next)
	var d Diff
	for name, s := range after {
		old, ok := before[name]
		if ok && old.Description == s.Description && old.Invocation == s.Invocation {
			continue
		}
		if ok && old.Invocation != s.Invocation {
			d.remove(old)
		}
		if s.Invocation == config.Model {
			d.AddTools = append(d.AddTools, s)
		} else {
			d.AddPrompts = append(d.AddPrompts, s)
		}
	}
	for name, old := range before {
		if _, ok := after[name]; !ok {
			d.remove(old)
		}
	}
	byName := func(a, b *Skill) int { return cmp.Compare(a.Name, b.Name) }
	slices.SortFunc(d.AddTools, byName)
	slices.SortFunc(d.AddPrompts, byName)
	slices.Sort(d.RemoveTools)
	slices.Sort(d.RemovePrompts)
	return d
}

func (d *Diff) remove(s *Skill) {
	if s.Invocation == config.Model {
		d.RemoveTools = append(d.RemoveTools, s.Name)
	} else {
		d.RemovePrompts = append(d.RemovePrompts, s.Name)
	}
}

func definitions(v *View) map[string]*Skill {
	out := map[string]*Skill{}
	if v == nil {
		return out
	}
	for _, s := range v.Skills {
		out[s.Name] = s
	}
	return out
}
