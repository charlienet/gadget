package logger

type Option func(*Options)

type Options struct {
	Level Level
}

func WithLevel(level Level) Option {
	return func(args *Options) {
		args.Level = level
	}
}
