package block

import (
	"crypto/cipher"
	"errors"

	"github.com/charlienet/gadget/crypto/padding"
)

type BlockCipher struct {
	Padding padding.Padding
}

func (b BlockCipher) ECBEncrypt(block cipher.Block, data []byte) ([]byte, error) {
	bs := block.BlockSize()
	data = b.Padding.Padding(data, bs)

	out := make([]byte, len(data))
	dst := out
	for len(data) > 0 {
		block.Encrypt(dst, data[:bs])
		data = data[bs:]
		dst = dst[bs:]
	}

	return out, nil
}

func (b BlockCipher) ECBDecrypt(block cipher.Block, cipherText []byte) ([]byte, error) {
	bs := block.BlockSize()
	if len(cipherText)%bs != 0 {
		return nil, errors.New("DecryptDES crypto/cipher: input not full blocks")
	}

	out := make([]byte, len(cipherText))
	dst := out
	for len(cipherText) > 0 {
		block.Decrypt(dst, cipherText[:bs])
		cipherText = cipherText[bs:]
		dst = dst[bs:]
	}

	return out, nil
}

func (b BlockCipher) CBCEncrypt(block cipher.BlockMode, data []byte) ([]byte, error) {
	data = b.Padding.Padding(data, block.BlockSize())
	out := make([]byte, len(data))

	block.CryptBlocks(out, data)
	return out, nil
}

func (b BlockCipher) CBCDecrypt(block cipher.BlockMode, cipherText []byte) ([]byte, error) {
	out := make([]byte, len(cipherText))
	block.CryptBlocks(out, cipherText)
	return b.Padding.UnPadding(out), nil
}
