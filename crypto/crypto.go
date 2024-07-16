package crypto

// 对称加密
type ISymmetric interface {
	Encrypt(msg []byte) ([]byte, error)
	Decrypt(cipherText []byte) ([]byte, error)
}

// 非对称加密
type IAsymmetric interface {
	Encrypt(msg []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

type Signer interface {
	Sign(msg []byte) ([]byte, error)
	Verify(msg, sign []byte) bool
}
