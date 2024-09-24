package cache

type Listener interface {
	Subscribe() chan string
	Publish(key string) error
	Close()
}
