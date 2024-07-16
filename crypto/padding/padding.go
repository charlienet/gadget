package padding

import (
	"bytes"

	"github.com/charlienet/gadget/misc/rand"
)

// 将字节数组填充到指定长度
// PKCS7	填充连续一样的值，并且值和长度相等
// ISO10126	最后一个字节为长度，其它填充个随机值
// ANSIX923	最后一个字节为长度，其它填充0

type Padding interface {
	Padding(src []byte, blockSize int) []byte
	UnPadding(src []byte) []byte
}

type padding struct{}

type pkcs7 struct{ padding }

type iso10126 struct{ padding }

type ansix923 struct{ padding }

var (
	PKCS7    = pkcs7{}
	ISO10126 = iso10126{}
	ANSIX923 = ansix923{}
)

func (p pkcs7) Padding(src []byte, blockSize int) []byte {
	padding := blockSize - len(src)%blockSize
	return append(src, p.pad(padding)...)
}

func (pkcs7) pad(padding int) []byte {
	return bytes.Repeat([]byte{byte(padding)}, padding)
}

func (i iso10126) Padding(src []byte, blockSize int) []byte {
	padding := blockSize - len(src)%blockSize
	return append(src, i.pad(padding)...)
}

func (iso10126) pad(padding int) []byte {
	pad := make([]byte, 0, padding)

	if padding > 0 {
		if padding > 1 {
			r, _ := rand.RandBytes(padding - 1)
			pad = append(pad, r...)
		}

		pad = append(pad, byte(padding))
	}

	return pad
}

func (i ansix923) Padding(src []byte, blockSize int) []byte {
	padding := blockSize - len(src)%blockSize

	return append(src, i.pad(padding)...)
}

func (ansix923) pad(padding int) []byte {
	pad := make([]byte, 0, padding)

	if padding > 0 {
		if padding > 1 {
			pad = append(pad, bytes.Repeat([]byte{0}, padding-1)...)
		}

		pad = append(pad, byte(padding))
	}

	return pad
}

func (padding) UnPadding(src []byte) []byte {
	length := len(src)
	unpadding := int(src[length-1])
	return src[:(length - unpadding)]
}

func ZeroPadding(ciphertext []byte, blockSize int) []byte {
	padding := blockSize - len(ciphertext)%blockSize
	padtext := bytes.Repeat([]byte{0}, padding)
	return append(ciphertext, padtext...)
}

func ZeroUnPadding(origData []byte) []byte {
	return bytes.TrimRightFunc(origData, func(r rune) bool {
		return r == rune(0)
	})
}
