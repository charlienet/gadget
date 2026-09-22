package idgenerator

import "strconv"

type ID int64

func (i ID) String() string {
	if i == 0 {
		return ""
	}

	return strconv.FormatInt(int64(i), 10)
}

func (i ID) Int64() int64 {
	return int64(i)
}
