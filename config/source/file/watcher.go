package file

import "github.com/fsnotify/fsnotify"

type watcher struct {
	f  *file
	fw *fsnotify.Watcher
}

func newWatcher(f *file) (*watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	_ = fw.Add(f.path)

	return &watcher{
		f:  f,
		fw: fw,
	}, nil
}

func (w *watcher) Stop() {
	_ = w.fw.Close()
}
