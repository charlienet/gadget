package logger

import (
	"io"
	"os"
)

type Option func(*Options)

type Options struct {
	Out   io.Writer
	Level Level
}

func New(recorder LogRecorder, opts ...Option) Logger {
	opt := Options{
		Level: Info,
		Out:   os.Stderr,
	}

	for _, o := range opts {
		o(&opt)
	}

	recorder.Init(opt)

	return newHelper(opt, recorder)
}

func WithLevel(level Level) Option {
	return func(o *Options) {
		o.Level = level
	}
}

func WithOutput(output io.Writer) Option {
	return func(o *Options) {
		o.Out = output
	}
}
