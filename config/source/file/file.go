package file

type file struct {
	path string
}

func New() file {
	return file{}
}

func (f *file) Watch() (*watcher, error) {
	return newWatcher(f)
}
