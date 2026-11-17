package config

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const (
	// DefaultPort is the edge port LocalStack listens on.
	DefaultPort = 4566

	// DefaultLocalstackHost is the host LOCALSTACK_HOST defaults to.
	DefaultLocalstackHost = "localhost.localstack.cloud"

	maxPort = 1<<16 - 1

	ipv4Wildcard = "0.0.0.0"
	ipv6Wildcard = "::"
)

// HostPort is a host and a port, as in GATEWAY_LISTEN and LOCALSTACK_HOST.
type HostPort struct {
	// Host is a hostname or an IP address without brackets.
	Host string
	// Port is the TCP port.
	Port int
}

// String returns the address as host:port, with IPv6 hosts in brackets.
func (h HostPort) String() string {
	return net.JoinHostPort(h.Host, strconv.Itoa(h.Port))
}

// ParseHostPort parses the forms LocalStack's HostAndPort.parse accepts:
// "0.0.0.0:4566", "0.0.0.0", ":4566", "[::]:4566" and "[::1]". A missing host or
// port takes the given default.
func ParseHostPort(input, defaultHost string, defaultPort int) (HostPort, error) {
	hostPort := HostPort{Host: defaultHost, Port: defaultPort}
	switch {
	case strings.HasPrefix(input, "["):
		err := parseBracketed(input, &hostPort)
		if err != nil {
			return HostPort{}, err
		}
	case strings.Contains(input, ":"):
		host, portText, _ := strings.Cut(input, ":")
		if host = strings.TrimSpace(host); host != "" {
			hostPort.Host = host
		}
		port, err := parsePort(portText)
		if err != nil {
			return HostPort{}, err
		}
		hostPort.Port = port
	default:
		if host := strings.TrimSpace(input); host != "" {
			hostPort.Host = host
		}
	}
	if hostPort.Port < 0 || hostPort.Port > maxPort {
		return HostPort{}, fmt.Errorf("port %d out of range", hostPort.Port)
	}

	return hostPort, nil
}

// parseBracketed fills hostPort from "[host]" or "[host]:port".
func parseBracketed(input string, hostPort *HostPort) error {
	end := strings.Index(input, "]")
	if end < 0 {
		return fmt.Errorf("%q looks like an IPv6 address but has no closing bracket", input)
	}
	host, rest := input[1:end], input[end+1:]
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.Is6() || addr.Zone() != "" {
		return fmt.Errorf("%q is not a valid IPv6 address in %q", host, input)
	}
	hostPort.Host = host
	switch {
	case rest == "":
	case strings.HasPrefix(rest, ":"):
		port, err := parsePort(rest[1:])
		if err != nil {
			return err
		}
		hostPort.Port = port
	default:
		return fmt.Errorf("%q is not formatted as [ip]:port", input)
	}

	return nil
}

func parsePort(text string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, fmt.Errorf("port %q is not a number", text)
	}

	return port, nil
}

// uniqueHostPorts dedupes bindings the way LocalStack's UniqueHostAndPortList
// does. "::" covers every other binding on its port, and "0.0.0.0" covers every
// other IPv4 binding, and identical bindings collapse. Order follows first
// appearance of each port, then of each host.
func uniqueHostPorts(in []HostPort) []HostPort {
	if len(in) <= 1 {
		return in
	}
	var ports []int
	hostsByPort := make(map[int][]string)
	for _, hostPort := range in {
		if _, seen := hostsByPort[hostPort.Port]; !seen {
			ports = append(ports, hostPort.Port)
		}
		hostsByPort[hostPort.Port] = append(hostsByPort[hostPort.Port], hostPort.Host)
	}
	out := make([]HostPort, 0, len(in))
	for _, port := range ports {
		hosts := hostsByPort[port]
		switch {
		case slices.Contains(hosts, ipv6Wildcard):
			out = append(out, HostPort{Host: ipv6Wildcard, Port: port})
		case slices.Contains(hosts, ipv4Wildcard):
			out = append(out, HostPort{Host: ipv4Wildcard, Port: port})
		default:
			var seen []string
			for _, host := range hosts {
				if !slices.Contains(seen, host) {
					seen = append(seen, host)
					out = append(out, HostPort{Host: host, Port: port})
				}
			}
		}
	}

	return out
}
