package logrus

import (
	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/sirupsen/logrus"
)

type Options struct {
	Formatter logrus.Formatter
}

type option func(*Options)

func WithTextFormatter() option {
	return func(o *Options) {
		o.Formatter = &logrus.TextFormatter{}
	}
}

func WithJSONFormatter() option {
	return func(o *Options) {
		o.Formatter = &logrus.JSONFormatter{}
	}
}

func WithNestedFormatter(fieldsOrder ...string) option {
	return func(o *Options) {
		o.Formatter = &nested.Formatter{
			FieldsOrder:     fieldsOrder,
			TimestampFormat: "2006-01-02 15:04:05.000"}
	}
}

func WithFormatter(formatter logrus.Formatter) option {
	return func(o *Options) {
		o.Formatter = formatter
	}
}
