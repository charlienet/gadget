package config

type Options struct {
	Source []ISource
}

type option func(*Options)

func WithSource(s ISource) option {
	return func(o *Options) {
		o.Source = append(o.Source, s)
	}
}
