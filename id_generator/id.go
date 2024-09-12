package idgenerator

import "strconv"

type Id int64

func (i Id) String() string {
	if i == 0 {
		return ""
	}

	return strconv.FormatInt(int64(i), 10)
}

func (i Id) Int64() int64 {
	return int64(i)
}
