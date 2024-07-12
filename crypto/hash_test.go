package crypto_test

import (
	"testing"

	"github.com/charlienet/gadget/crypto"
)

func TestHash(t *testing.T) {
	t.Log(crypto.SHA256.Sum([]byte("abc")).Hex())
	t.Log(crypto.SHA256.Sum([]byte("abc")).Base64())
	t.Log(crypto.SM3.Sum([]byte("abc")).Hex())
}

func TestHMac(t *testing.T) {
	t.Log(crypto.SM3.HMAC([]byte("abc")).Sum([]byte("abc")).Hex())
	t.Log(crypto.SHA256.HMAC([]byte("123456")).Sum([]byte("sample message")).Hex())
}
