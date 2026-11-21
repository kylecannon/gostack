package expression

// Clash is the pair findPathClash names for the tests of this package: One is
// the path being added, Two the path it names second.
type Clash struct {
	One, Two Path
	Conflict bool
}

// PathsClash is findPathClash for the tests of this package, nil when the paths
// do not clash.
func PathsClash(paths []Path) *Clash {
	c := findPathClash(paths)
	if c == nil {
		return nil
	}

	return &Clash{One: c.one, Two: c.two, Conflict: c.conflict}
}
