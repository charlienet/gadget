package crypto

import (
	"crypto/hmac"
	"hash"

	"github.com/charlienet/gadget/misc/bytesconv"
)

type HMAC uint

type hmacDigest struct {
	h hash.Hash
}

func (h Hash) HMAC(key []byte) hmacDigest {
	f := hashes[h]
	return hmacDigest{
		h: hmac.New(f, key),
	}
}

func (d hmacDigest) Sum(msg []byte) bytesconv.BytesResult {
	d.h.Write(msg)
	return d.h.Sum(nil)
}
