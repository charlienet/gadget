package store

import "time"

type Options struct {
	Prefix string
	Table  string
}

type OptionInterface interface {
	apply(Options)
}

type Option func(o *Options)

func WitePrefix(prefix string) Option {
	return func(o *Options) {
		o.Prefix = prefix
	}
}

type WriteOptions struct {
	Expiry time.Time
	TTL    time.Duration
}

type WriteOption func(w *WriteOptions)

func WriteExpiry(t time.Time) WriteOption {
	return func(w *WriteOptions) {
		w.Expiry = t
	}
}

func WithTTL(d time.Duration) WriteOption {
	return func(w *WriteOptions) {
		w.TTL = d
	}
}
