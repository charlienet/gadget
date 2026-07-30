package logrus

import (
	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/sirupsen/logrus"
)

type Options struct {
	Formatter   logrus.Formatter
	Hooks       []logrus.Hook
	ReportCaller bool
}

type Option func(*Options)

func WithTextFormatter() Option {
	return func(o *Options) {
		o.Formatter = &logrus.TextFormatter{}
	}
}

func WithJSONFormatter() Option {
	return func(o *Options) {
		o.Formatter = &logrus.JSONFormatter{}
	}
}

func WithNestedFormatter(fieldsOrder ...string) Option {
	return func(o *Options) {
		o.Formatter = &nested.Formatter{
			FieldsOrder:     fieldsOrder,
			TimestampFormat: "2006-01-02 15:04:05.000"}
	}
}

func WithFormatter(formatter logrus.Formatter) Option {
	return func(o *Options) {
		o.Formatter = formatter
	}
}

// WithReportCaller enables caller reporting in logrus.
func WithReportCaller() Option {
	return func(o *Options) {
		o.ReportCaller = true
	}
}

// WithHook attaches a logrus hook to the logger.
func WithHook(hook logrus.Hook) Option {
	return func(o *Options) {
		o.Hooks = append(o.Hooks, hook)
	}
}
