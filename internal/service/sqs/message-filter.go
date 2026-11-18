package sqs

import (
	"slices"
	"strconv"
	"strings"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
)

// The names of a message attribute filter that mean "every attribute", and the
// suffix that makes a name a prefix.
const (
	filterAll     = "All"
	filterDotStar = ".*"
	filterStar    = "*"
)

// intAttr reads an attribute that holds a whole number, as the queue's lock
// holds it. An attribute that is not a number, which LocalStack does not check
// for, reads as the default of a new queue.
func (q *Queue) intAttr(name string) int {
	value, err := strconv.Atoi(q.attrs[name])
	if err != nil {
		value, _ = strconv.Atoi(fallbackAttributes[name])
	}

	return value
}

// fallbackAttributes are what intAttr reads for an attribute that is not a
// number: the default of a new queue. An attribute that is missing from the map
// reads as 0.
var fallbackAttributes = map[string]string{
	attrVisibilityTimeout:      defaultVisibilityTimeout,
	attrMaximumMessageSize:     defaultMaximumMessageSize,
	attrMessageRetentionPeriod: defaultRetentionPeriod,
}

// filterSystemAttributes returns the system attributes of a delivery that names
// asks for, all of them for "All". Asking for none returns none.
func filterSystemAttributes(m *message, d *delivery, names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}

	all := slices.Contains(names, filterAll)
	out := make(map[string]string, len(m.system)+2) //nolint:mnd // the two attributes of a receive.

	add := func(name, value string) {
		if all || slices.Contains(names, name) {
			out[name] = value
		}
	}

	for name, value := range m.system {
		add(name, value)
	}

	add(sysApproximateReceiveCount, strconv.Itoa(d.receiveCount))
	add(sysApproximateFirstReceiveTimestamp, strconv.FormatInt(d.firstReceived.UnixMilli(), 10))

	return out
}

// filterMessageAttributes returns the user attributes a receiver asked for: all
// with "All", ".*" or "*", otherwise the ones named and the ones that start
// with the text before the ".*" of a prefix name.
func filterMessageAttributes(
	attrs map[string]sqsapi.MessageAttributeValue, names []string,
) map[string]sqsapi.MessageAttributeValue {
	if len(attrs) == 0 || len(names) == 0 {
		return nil
	}

	if slices.ContainsFunc(names, func(name string) bool {
		return name == filterAll || name == filterDotStar || name == filterStar
	}) {
		return attrs
	}

	var prefixes []string

	exact := make([]string, 0, len(names))

	for _, name := range names {
		if prefix, found := strings.CutSuffix(name, filterDotStar); found {
			prefixes = append(prefixes, prefix)
		} else {
			exact = append(exact, name)
		}
	}

	matched := make(map[string]sqsapi.MessageAttributeValue)

	for name, value := range attrs {
		hasPrefix := func(prefix string) bool { return strings.HasPrefix(name, prefix) }
		if slices.Contains(exact, name) || slices.ContainsFunc(prefixes, hasPrefix) {
			matched[name] = value
		}
	}

	if len(matched) == 0 {
		return nil
	}

	return matched
}
