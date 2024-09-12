package env

import (
	"os"
	"strings"
)

// StringReplacer applies a set of replacements to a string.
type StringReplacer interface {
	// Replace returns a copy of s with all replacements performed.
	Replace(s string) string
}

type env struct {
	prefixes       []string
	envKeyReplacer StringReplacer
}

func New() *env {
	return &env{}
}

func (e *env) Read() error {
	for _, env := range os.Environ() {
		println(env)

		pair := strings.SplitN(env, "=", 2)
		value := pair[1]

		keys, ok := e.getEnv(pair[0])

		println("keys:", keys, ok)
		_ = value
	}

	return nil
}

func (e *env) getEnv(key string) (string, bool) {
	if e.envKeyReplacer != nil {
		key = e.envKeyReplacer.Replace(key)
	}

	val, ok := os.LookupEnv(key)
	return val, ok
}
