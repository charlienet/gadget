package sm4

import (
	"errors"
	"strconv"
	"sync"

	"github.com/charlienet/gadget/misc/rand"
	s4 "github.com/tjfoc/gmsm/sm4"
)

const (
	BlockSize = s4.BlockSize
)

func GenerateKey() []byte {
	k, _ := rand.RandBytes(BlockSize)
	return k
}

func ECB(key []byte) ecb {
	return ecb{key: key}
}

type cbcOption func(*cbc) error

func CBC(key []byte, opts ...cbcOption) (*cbc, error) {
	if len(key) != BlockSize {
		return nil, errors.New("SM4: invalid key size " + strconv.Itoa(len(key)))
	}

	c := &cbc{key: key}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	return &cbc{key: key}, nil
}

func WithIV(iv []byte) cbcOption {
	return func(c *cbc) error {
		if len(iv) != BlockSize {
			return errors.New("SM4: invalid iv size " + strconv.Itoa(len(iv)))
		}

		c.iv = iv
		return nil
	}
}

type ecb struct {
	key []byte
}

func (o ecb) Encrypt(msg []byte) ([]byte, error) {
	return s4.Sm4Ecb(o.key, msg, true)
}

func (o ecb) Dectypt(in []byte) ([]byte, error) {
	return s4.Sm4Ecb(o.key, in, false)
}

type cbc struct {
	key []byte
	iv  []byte
}

var lock sync.Mutex

func (c cbc) Encrypt(in []byte) ([]byte, error) {
	return c.cbc(in, true)
}

func (c cbc) Dectypt(in []byte) ([]byte, error) {
	return c.cbc(in, false)
}

func (c cbc) cbc(in []byte, mode bool) ([]byte, error) {
	lock.Lock()
	defer lock.Unlock()

	s4.SetIV(c.iv)
	defer s4.SetIV(emptyIV)

	return s4.Sm4Cbc(c.key, in, mode)
}

var emptyIV = make([]byte, s4.BlockSize)
