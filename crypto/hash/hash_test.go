package hash_test

import (
	"testing"

	"github.com/charlienet/gadget/crypto/hash"
)

func TestHash(t *testing.T) {
	t.Log(hash.SHA256.Sum([]byte("abc")).Hex())
	t.Log(hash.SHA256.Sum([]byte("abc")).Base64())
	t.Log(hash.SM3.Sum([]byte("abc")).Hex())
}

func TestHMac(t *testing.T) {
	t.Log(hash.SM3.HMAC([]byte("abc")).Sum([]byte("abc")).Hex())
	t.Log(hash.SHA256.HMAC([]byte("123456")).Sum([]byte("sample message")).Hex())
}
