package etcd

import (
	"context"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type watcher struct {
	exit chan bool
}

func newWatcher(prefix string, wc clientv3.Watcher) *watcher {
	w := &watcher{}

	ch := wc.Watch(context.Background(), "", clientv3.WithPrefix())
	go w.run(wc, ch)

	return w
}

func (w *watcher) run(wc clientv3.Watcher, ch clientv3.WatchChan) {
	for {
		select {
		case resp, ok := <-ch:
			if !ok {
				return
			}
			_ = resp
		case <-w.exit:
			wc.Close()
			return
		}
	}
}

func (w *watcher) Stop() error {
	select {
	case <-w.exit:
		return nil
	default:
		close(w.exit)
	}

	return nil
}
