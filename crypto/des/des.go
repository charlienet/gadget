package des

import (
	"crypto/cipher"
	"crypto/des"
	"errors"

	"github.com/charlienet/gadget/crypto/internal/block"
	"github.com/charlienet/gadget/crypto/padding"
)

type ecb struct {
	cipher block.BlockCipher
	block  cipher.Block
}

type cbc struct {
	cipher block.BlockCipher
	block  cipher.Block
	iv     []byte
}

func ECB(key []byte) (*ecb, error) {
	b, err := newCipher(key)
	if err != nil {
		return nil, err
	}

	return &ecb{block: b, cipher: block.BlockCipher{Padding: padding.PKCS7}}, nil
}

func (e ecb) Encrypt(in []byte) ([]byte, error) {
	return e.cipher.ECBEncrypt(e.block, in)
}

func (e ecb) Decrypt(in []byte) ([]byte, error) {
	return e.cipher.ECBDecrypt(e.block, in)
}

type cbcOption = func(*cbc) error

func WithIV(iv []byte) cbcOption {
	return func(c *cbc) error {
		if len(iv) != c.block.BlockSize() {
			return errors.New("DES: invalid iv size ")
		}

		c.iv = iv

		return nil
	}
}

func CBC(key []byte, opts ...cbcOption) (*cbc, error) {
	b, err := newCipher(key)
	if err != nil {
		return nil, err
	}

	c := &cbc{block: b, cipher: block.BlockCipher{Padding: padding.PKCS7}}

	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	if len(c.iv) == 0 {
		c.iv = make([]byte, c.block.BlockSize())
	}

	return c, nil
}

func (c cbc) Encrypt(in []byte) ([]byte, error) {
	e := cipher.NewCBCEncrypter(c.block, c.iv)
	return c.cipher.CBCEncrypt(e, in)
}

func (c cbc) Decrypt(in []byte) ([]byte, error) {
	d := cipher.NewCBCDecrypter(c.block, c.iv)
	return c.cipher.CBCDecrypt(d, in)
}

func newCipher(key []byte) (cipher.Block, error) {
	switch len(key) {
	case 8:
		return des.NewCipher(key)
	case 24:
		return des.NewTripleDESCipher(key)
	default:
		return nil, des.KeySizeError(len(key))
	}
}
