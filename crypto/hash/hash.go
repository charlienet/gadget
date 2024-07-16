package hash

import (
	"crypto"
	"hash"
	"hash/fnv"

	"github.com/charlienet/gadget/misc/bytesconv"
	"github.com/tjfoc/gmsm/sm3"
)

type Hash uint

const (
	MD4         Hash = 1 + iota // import golang.org/x/crypto/md4
	MD5                         // import crypto/md5
	SHA1                        // import crypto/sha1
	SHA224                      // import crypto/sha256
	SHA256                      // import crypto/sha256
	SHA384                      // import crypto/sha512
	SHA512                      // import crypto/sha512
	MD5SHA1                     // no implementation; MD5+SHA1 used for TLS RSA
	RIPEMD160                   // import golang.org/x/crypto/ripemd160
	SHA3_224                    // import golang.org/x/crypto/sha3
	SHA3_256                    // import golang.org/x/crypto/sha3
	SHA3_384                    // import golang.org/x/crypto/sha3
	SHA3_512                    // import golang.org/x/crypto/sha3
	SHA512_224                  // import crypto/sha512
	SHA512_256                  // import crypto/sha512
	BLAKE2s_256                 // import golang.org/x/crypto/blake2s
	BLAKE2b_256                 // import golang.org/x/crypto/blake2b
	BLAKE2b_384                 // import golang.org/x/crypto/blake2b
	BLAKE2b_512                 // import golang.org/x/crypto/blake2b
	SM3                         // import github.com/tjfoc/gmsm/sm3
	maxHash
)

var hashes = map[Hash]func() hash.Hash{
	MD4:         crypto.MD4.New,
	MD5:         crypto.MD5.New,
	SHA1:        crypto.SHA1.New,
	SHA224:      crypto.SHA224.New,
	SHA256:      crypto.SHA256.New,
	SHA384:      crypto.SHA384.New,
	SHA512:      crypto.SHA512.New,
	MD5SHA1:     crypto.MD5SHA1.New,
	RIPEMD160:   crypto.RIPEMD160.New,
	SHA3_224:    crypto.SHA3_224.New,
	SHA3_256:    crypto.SHA3_256.New,
	SHA3_384:    crypto.SHA3_384.New,
	SHA3_512:    crypto.SHA3_512.New,
	SHA512_224:  crypto.SHA512_224.New,
	SHA512_256:  crypto.SHA512_256.New,
	BLAKE2s_256: crypto.BLAKE2b_256.New,
	BLAKE2b_256: crypto.BLAKE2b_256.New,
	BLAKE2b_384: crypto.BLAKE2b_384.New,
	BLAKE2b_512: crypto.BLAKE2b_512.New,
	SM3:         sm3.New,
}

func (h Hash) GetHash() hash.Hash {
	c := hashes[h]
	return c()
}

func (h Hash) Sum(s []byte) bytesconv.BytesResult {
	c := hashes[h]

	hash := c()
	hash.Write(s)
	return hash.Sum(nil)
}

func Funv32(msg []byte) uint32 {
	h := fnv.New32()
	h.Write(msg)
	return h.Sum32()
}

func Funv64(msg []byte) uint64 {
	h := fnv.New64()
	h.Write(msg)
	return h.Sum64()
}
