package bytesconv

import (
	"encoding/base64"
	"encoding/hex"
)

const hexTable = "0123456789ABCDEF"

type BytesResult []byte

func FromString(s string) BytesResult {
	return (BytesResult)([]byte(s))
}

func FromBytes(b []byte) BytesResult {
	return BytesResult(b)
}

// FromHexString 从十六进制获取
func FromHexString(s string) (BytesResult, error) {
	b, err := hex.DecodeString(s)
	return BytesResult(b), err
}

func FromBase64String(s string) (BytesResult, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	return BytesResult(b), err
}

func (r BytesResult) Hex() string {
	return hex.EncodeToString(r)
}

func (r BytesResult) UppercaseHex() string {
	dst := make([]byte, hex.EncodedLen(len(r)))
	j := 0

	re := r[:]
	for _, v := range re {
		dst[j] = hexTable[v>>4]
		dst[j+1] = hexTable[v&0x0f]
		j += 2
	}

	return BytesToString(dst)
}

func (r BytesResult) Base64() string {
	return base64.StdEncoding.EncodeToString(r)
}

func (r BytesResult) Bytes() []byte {
	return r
}

func (r BytesResult) String() string {
	return r.Hex()
}
