package sm2

import (
	"crypto/rand"
	"errors"

	s "github.com/tjfoc/gmsm/sm2"
	x "github.com/tjfoc/gmsm/x509"
)

var (
	defaultMode = C1C3C2
	C1C3C2      = 0
	C1C2C3      = 1
)

type option func(*sm2Instance) error

type sm2Instance struct {
	mode int
	prk  *s.PrivateKey
	puk  *s.PublicKey
}

func WithSm2PrivateKey(p []byte, pwd []byte) option {
	return func(so *sm2Instance) error {
		priv, err := x.ReadPrivateKeyFromPem(p, pwd)
		if err != nil {
			return err
		}

		so.prk = priv
		return nil
	}
}

func WithSm2PublicKey(p []byte) option {
	return func(so *sm2Instance) error {
		if len(p) == 0 {
			return nil
		}

		pub, err := x.ReadPublicKeyFromPem(p)
		if err != nil {
			return err
		}

		so.puk = pub
		return nil
	}
}

func WithMode(mode int) option {
	return func(so *sm2Instance) error {
		so.mode = mode
		return nil
	}
}

func New(opts ...option) (*sm2Instance, error) {
	o := &sm2Instance{
		mode: defaultMode,
	}

	for _, f := range opts {
		if err := f(o); err != nil {
			return o, err
		}
	}

	if o.prk == nil {
		priv, err := s.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}

		o.prk = priv
	}

	if o.puk == nil {
		o.puk = &o.prk.PublicKey
	}

	return o, nil
}

func (o *sm2Instance) Encrypt(msg []byte) ([]byte, error) {
	return s.Encrypt(o.puk, msg, rand.Reader, o.mode)
}

func (o *sm2Instance) Decrypt(cipherText []byte) ([]byte, error) {
	return s.Decrypt(o.prk, cipherText, o.mode)
}

func (o *sm2Instance) Sign(msg []byte) ([]byte, error) {
	if o.prk == nil {
		return []byte{}, errors.New("private key is nil")
	}

	b, e := o.prk.Sign(rand.Reader, msg, nil)
	return b, e
}

func (o *sm2Instance) Verify(msg []byte, sign []byte) bool {
	if o.puk == nil {
		return false
	}

	return o.puk.Verify(msg, sign)
}
