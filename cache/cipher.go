package cache

// Cipher 提供透明的数据加解密。注入后：缓存（L1/L2）中存储的是 Encrypt 的结果
// （密文），Get/Getfn/GetMulti 返回前经 Decrypt 还原——调用方 Put/Get 无感知
// （明文进出）。nil 表示不加密。
//
// 实现由应用注入（依赖方向应用 → cache），cache 包不 import 任何加密库约定之外
// 的数据源。实现必须并发安全（同一实例可能被多个 goroutine 并发调用）。
type Cipher interface {
	// Encrypt 加密明文。返回的密文将原样写入缓存（含版本前缀）。
	Encrypt(plaintext []byte) ([]byte, error)
	// Decrypt 解密密文，还原为明文（序列化后的字节）。
	Decrypt(ciphertext []byte) ([]byte, error)
}
