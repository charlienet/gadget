package rotate

import (
	"strconv"
	"testing"
)

func TestWrite(t *testing.T) {
	w := &RotateDateWriter{Filename: "logs/av.log"}

	for i := range 100 {
		w.Write([]byte("abc" + strconv.Itoa(i) + "\n"))
	}
}

func TestMuti(t *testing.T) {
	w := &RotateDateWriter{Filename: "logs/av.test.log"}
	for i := range 10 {
		w.Write([]byte("abc" + strconv.Itoa(i) + "\n"))
	}
}

func TestNoExt(t *testing.T) {
	w := &RotateDateWriter{Filename: "logs/av"}
	w.Write([]byte("noext"))
}

func TestHideFile(t *testing.T) {
	w := &RotateDateWriter{Filename: ".hidefile"}
	w.Write([]byte("hidefile"))
}

func TestNoFileName(t *testing.T) {
	w := &RotateDateWriter{}

	w.Write([]byte("abc11"))
}

func BenchmarkWrite(b *testing.B) {
	w := &RotateDateWriter{Filename: "logs/aaaa.log"}

	for i := 0; b.Loop(); i++ {
		w.Write([]byte("abc" + strconv.Itoa(i) + "\n"))
	}
}
