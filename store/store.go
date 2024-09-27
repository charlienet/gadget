package store

import "errors"

var (
	ErrNotFount = errors.New("not found")
)

type Store interface {
	Read(key string) error
	Write(key string, v any, opts ...WriteOption) error
	Delete(key string) error
	Close() error
	String() string
}
