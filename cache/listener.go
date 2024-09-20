package cache

type Listener interface {
	Subscribe(key string) chan string
	Publish(key string) error
	Close()
}
