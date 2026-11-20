package expression

// Names returns the top-level attribute name of every document path c reads,
// in the order the expression names them, with #name placeholders already
// substituted. Query uses it to refuse a FilterExpression on a key attribute.
func Names(c Condition) []string {
	cc, ok := c.(*condition)
	if !ok {
		return nil
	}

	var names []string

	_ = walkOperands(cc.root, func(o *operand) error {
		collectNames(o, &names)

		return nil
	})

	return names
}

// collectNames appends the top-level name of o, or of every path among a
// function's arguments.
func collectNames(o *operand, names *[]string) {
	switch o.kind {
	case opPath:
		if len(o.path) > 0 {
			*names = append(*names, o.path[0].Name)
		}
	case opFunc:
		for _, arg := range o.args {
			collectNames(arg, names)
		}
	default:
	}
}
