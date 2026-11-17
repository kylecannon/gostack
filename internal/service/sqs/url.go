package sqs

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/kylecannon/gostack/internal/account"
)

const (
	// defaultScheme is the scheme of every URL that does not take it from the
	// request. LocalStack takes it from USE_SSL, which gostack has no use for:
	// one port serves HTTP and TLS.
	defaultScheme = "http"
	// domainRegion is the region that the domain strategy leaves out of the
	// host, and that a host without a region stands for.
	domainRegion = account.DefaultRegion

	pathPrefix = "queue"
	// withRegionParts is the number of path segments of a queue URL that has the
	// /queue/<region> prefix. Without it there are two.
	withRegionParts = 4
	// bareParts is the number of path segments of /<account>/<queue>.
	bareParts = 2
)

// queueURL returns the URL of q for a caller that reached gostack with r,
// following SQS_ENDPOINT_STRATEGY the way LocalStack's SqsQueue.url does.
func queueURL(settings *Settings, r *http.Request, q *Queue) string {
	scheme := defaultScheme
	hostAndPort := settings.Host
	strategy := settings.EndpointStrategy

	if strategy == StrategyDynamic {
		scheme = requestScheme(r)
		strategy, hostAndPort = guessStrategy(r.Host)
	}

	var base string

	switch strategy {
	case StrategyStandard:
		// The region is always part of the host, and the scheme is the
		// request's.
		base = requestScheme(r) + "://sqs." + q.region + "." + hostAndPort
	case StrategyDomain:
		region := ""
		if q.region != domainRegion {
			region = q.region + "."
		}

		base = scheme + "://" + region + "queue." + hostAndPort
	case StrategyPath:
		base = scheme + "://" + hostAndPort + "/" + pathPrefix + "/" + q.region
	default:
		base = scheme + "://" + hostAndPort
	}

	return strings.TrimRight(base, "/") + "/" + q.account + "/" + q.name
}

// requestScheme is the scheme a caller used: https when it spoke TLS.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}

	return defaultScheme
}

// guessStrategy is LocalStack's guess_endpoint_strategy_and_host: it reads the
// strategy a Host header was built for, and returns the part of the host after
// the endpoint prefix and region.
func guessStrategy(host string) (string, string) {
	components := strings.Split(host, ".")

	switch {
	case strings.HasPrefix(host, "sqs."):
		return StrategyStandard, strings.Join(components[2:], ".")
	case strings.HasPrefix(host, "queue."):
		return StrategyDomain, strings.Join(components[1:], ".")
	case len(components) > 2 && components[1] == pathPrefix:
		return StrategyDomain, strings.Join(components[2:], ".")
	default:
		return StrategyPath, host
	}
}

// location is the account, region and name a queue URL points at. The region is
// empty when the URL does not carry one.
type location struct {
	account string
	region  string
	name    string
}

// parseQueueURL is LocalStack's parse_queue_url. ok is false for a string that
// is not a queue URL, which LocalStack then takes as a queue name.
func parseQueueURL(queueURL string) (location, bool) {
	parsed, err := url.Parse(strings.TrimRight(queueURL, "/"))
	if err != nil {
		return location{}, false
	}

	parts := strings.Split(strings.TrimLeft(parsed.EscapedPath(), "/"), "/")
	if len(parts) != bareParts && len(parts) != withRegionParts {
		return location{}, false
	}

	loc := location{account: parts[len(parts)-2], name: parts[len(parts)-1]}
	domain := strings.Split(parsed.Host, ".")

	switch {
	case len(parts) == withRegionParts:
		if parts[0] != pathPrefix {
			return location{}, false
		}

		loc.region = parts[1]
	case strings.HasPrefix(parsed.Host, "sqs."):
		// "sqs." leaves at least two parts after the split.
		loc.region = domain[1]
	case strings.Contains(parsed.Host, ".queue."):
		if domain[1] != pathPrefix {
			return location{}, false
		}

		loc.region = domain[0]
	case strings.HasPrefix(parsed.Host, pathPrefix):
		loc.region = domainRegion
	default:
		// A URL without a region in it: the caller's region applies.
	}

	return loc, true
}
