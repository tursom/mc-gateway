package adminroute

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

func ValidateHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return errors.New("host is required")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return errors.New("host must not contain whitespace")
	}
	if strings.Contains(host, "/") {
		return errors.New("host must not contain /")
	}
	return nil
}

func ValidateUpstream(upstream string) error {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return errors.New("upstream is required")
	}

	for _, prefix := range []string{"kcp://", "quic://", "haproxy://"} {
		upstream = strings.TrimPrefix(upstream, prefix)
	}
	host, portValue, err := net.SplitHostPort(upstream)
	if err != nil {
		return errors.New("upstream must be host:port")
	}
	if strings.TrimSpace(host) == "" {
		return errors.New("upstream host is required")
	}
	port, err := strconv.Atoi(portValue)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("upstream port must be an integer from 1 to 65535")
	}
	return nil
}
