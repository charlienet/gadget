package aes

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"

	"github.com/charlienet/gadget/crypto/internal/block"
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
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	a := &ecb{block: block}
	return a, nil
}

func (c ecb) Encrypt(in []byte) ([]byte, error) {
	return c.cipher.ECBEncrypt(c.block, in)
}

func (c ecb) Decrypt(in []byte) ([]byte, error) {
	return c.cipher.ECBDecrypt(c.block, in)
}

type cbcOption func(*cbc) error

func WithIV(iv []byte) cbcOption {
	return func(c *cbc) error {
		if len(iv) != aes.BlockSize {
			return errors.New("IV length must equal block size")
		}

		c.iv = iv
		return nil
	}
}

func CBC(key []byte) (*cbc, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	a := &cbc{block: block}

	if len(a.iv) == 0 {
		a.iv = make([]byte, aes.BlockSize)
	}

	return a, nil
}

func (c cbc) Encrypt(in []byte) ([]byte, error) {
	entrypter := cipher.NewCBCEncrypter(c.block, c.iv)
	return c.cipher.CBCEncrypt(entrypter, in)
}

func (c cbc) Decrypt(in []byte) ([]byte, error) {
	mode := cipher.NewCBCDecrypter(c.block, c.iv)
	return c.cipher.CBCDecrypt(mode, in)
}
