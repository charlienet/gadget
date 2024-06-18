package logger

type Logger struct {
}

func New(opts ...Option) Logger {
	options := Options{
		Level: Info,
	}

	for _, o := range opts {
		o(&options)
	}

	return Logger{}
}
