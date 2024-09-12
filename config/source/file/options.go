package file

import "io/fs"

func WithPath(p string, filetype string) file {
	return New()
}

func WithFS(fs fs.FS, filetype string) {

}
