package redis

import (
	"strings"
)

const (
	defaultSeparator = ":"
)

type redisPrefix struct {
	prefix    string
	separator string
}

func newPrefix(separator string, prefix ...string) redisPrefix {
	s := defaultString(len(separator) > 0, separator, defaultSeparator)

	return redisPrefix{
		separator: s,
		prefix:    defaultString(len(prefix) > 0, strings.Join(prefix, separator), ""),
	}
}

func (p *redisPrefix) Prefix() string {
	return p.prefix
}

func (p *redisPrefix) Separator() string {
	return p.separator
}

func (p *redisPrefix) hasPrefix() bool {
	return len(p.prefix) > 0
}

func (p *redisPrefix) rename(key ...string) string {
	s := make([]string, 0, len(key)+1)
	if len(p.prefix) > 0 {
		s = append(s, p.prefix)
	}

	s = append(s, key...)

	return strings.Join(s, p.separator)
}

func (p *redisPrefix) renames(key ...string) []string {
	s := make([]string, 0, len(key))
	for _, k := range key {
		s = append(s, p.rename(k))
	}

	return s
}

func defaultString(cond bool, v, dv string) string {
	if cond {
		return v
	}

	return dv
}
