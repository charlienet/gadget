package rsa_test

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/charlienet/gadget/crypto/hash"
	"github.com/charlienet/gadget/crypto/rsa"
	"github.com/stretchr/testify/assert"
)

func TestRsaSign(t *testing.T) {
	rsa, err := rsa.New(hash.SHA256)
	t.Log(rsa, err)

	msg := []byte("123456")
	sign, err := rsa.Sign(msg)
	t.Log(base64.StdEncoding.EncodeToString(sign))
	t.Log(hex.EncodeToString(sign), err)

	t.Log(rsa.Verify(msg, sign))
}

func TestSM3Sign(t *testing.T) {
	c, err := rsa.New(hash.SM3)
	t.Log(err)
	t.Log(c.Sign([]byte("abc")))
}

func TestEncrypt(t *testing.T) {

	rsa, err := rsa.New(hash.SHA1)
	t.Log(rsa, err)

	msg := []byte("123456")
	cipherText, err := rsa.Encrypt(msg)
	t.Log(base64.StdEncoding.EncodeToString(cipherText), err)

	decrypted, err := rsa.Decrypt(cipherText)
	t.Log(string(decrypted), err)
}

const (
	pkBytes = `-----BEGIN RSA PRIVATE KEY-----
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
-----END RSA PRIVATE KEY-----`

	badPubBytes = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDWKn2UPL1SmlufOkgMJHDqLhjf
vyT55z2MZRzeLqB3u8YlRLUD8zi3kmQy6loHQFu0FR7en/DI9EWRXARxMKhbH+CM
yzwmdh9QlzpMrQk0p4e5VtM5pXX9d4B4XxKBXBrmp2n/1D3+oovzD6p37dqqsgPH
xOQ3KQNxnTteS00kzQIDAQAB
-----END PUBLIC KEY-----`
)

func TestParseKey(t *testing.T) {
	rsa, err := rsa.New(
		hash.SHA1,
		rsa.ParsePKCS1PrivateKey([]byte(pkBytes)))

	t.Log(rsa, err)

	msg := []byte("123456")
	sign, err := rsa.Sign(msg)
	t.Log(base64.StdEncoding.EncodeToString(sign))
	t.Log(hex.EncodeToString(sign), err)

	t.Log(rsa.Verify(msg, sign))
}

func TestBadPubKey(t *testing.T) {
	rsa, err := rsa.New(
		hash.SHA1,
		rsa.ParsePKCS1PrivateKey([]byte(pkBytes)),
		rsa.ParsePKIXPublicKey([]byte(badPubBytes)))

	t.Log(rsa, err)

	msg := []byte("123456")
	sign, err := rsa.Sign(msg)
	t.Log(base64.StdEncoding.EncodeToString(sign))
	t.Log(hex.EncodeToString(sign), err)

	t.Log(rsa.Verify(msg, sign))
}

func TestRsa2048Sign(t *testing.T) {
	key := `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEAzLWIDZbQUyQh1+SLWxE6pS3lttbHSZo+B5NRB6R5nonuAmyf
XqQyJfDAvb2DTxXFL1y+VR2uEoQj/rg40PJ54ZnxQpEONEgSR7seIQr4xoe6h8SC
W0TcSBIejkzPkGPkWjsXczV74yQxz3+23SGSWiojEeWgjvQ6oI7SUC+yPb9UvaAx
GwMFCc0H5opSHW3ZVPdU6Q6bLK7QZXhY5CstXcxJ+bFQq3MPf+Dv5pB4qSR8PX02
1pI/gmraEpbwU2Wqe+V07vA9ougJ/ON/xZtvuVT1xW3wazjC/QeBdWOjRhUsk611
mo0retZ7nv72Tln5zBX8qPlJYNX5FQdv+LmRLwIDAQABAoIBAQC/F+6rkM9j7WTS
XnxgRJMUQuYfahua76tb8v8/PSBzCJrwFGopNOnDVRz3goOjPdVWwyLB3fTzP/tB
+sK++rsgCE6ZL0OtNmIqJ9iWS+GzolxUBPMTNBLWDGQNvlI8naM7P9JCL/k4Lj95
TeVsQ7yVAqS+PjdFe2OHIgvd4shmrhttKYelXmjhoLi3SCIzU4Uj1RfBSzUM4Ffp
4FeuYCCxf2BSnweOa0DNREGlubMxcd+RQAChnDUbgdlpwjFJUlOmFYBCMn7U8ypG
PP3Z3/JI3glElVR7FQHS2yGvO4K9viHNr8eeDczKM+8VDs6ZT2ljpSXFei5Kv+Dv
tru4LlDxAoGBAOp8IarOlJnkDY4qoa0aN3ql0Z9yjCl7xy+7RytUjJvVLJbGkUpp
u6B+PJuVxa+OU4PKNSubU2gIcr+QaIo0tVyjZy2+zgSdddJLLc6q2wv7RqIsXdbk
PctfBAW+icu+ZSANyuI5bn220tAwQ9UO1F4bi/u/H64s6c5XN+i+9xn7AoGBAN99
/WmOOnRJSNOTUMHjT5NNi/rh4UwSajB8S5nPsYUVB9Yj8CLbICzY3RWQkz6xbdoZ
e86IVDrcGa0cPWZyw/ecDCGt6JbfsYpzFmD1iQtrtBU0SfvZV47uAlN5d7CtAGKS
82rGCTNI+E65iKQY6D4WBygHFU2vX41OHFdq/pNdAoGBAIQf7diRDqqoFftFilQ/
sYMqbDOsF85IMLR0kmWX/qLQO4+506Rab56/gucoPXvudqCMD+nCW/0CxaWreTxm
9sp8SGc+XFe9YeZc9jK9ky/tJp+64CV19lvh7iJOetaTMegd3XQbaGbt3Vvx1kb3
VDKy0u3Hg9Jg/F2IR7id4h6BAoGACs2sUk3txXFFc/TLEpRKZHR7L8V7fpHlUDKx
9N11V1mM520VTpoJFCHnjgNPGti41rIkqfctGytIknWrAijKEE4ayAYAGEr36hlm
G4nC9ipeqie868+1y9L1idN1VbUHL7yqx56LE0+TsTqGwGfz0gx+jBDLltXDaLE9
7XvekoECgYAc0QmX3E7NV/Ya41M1PAHA9/1caGa4nujKFtrKFUsTIhBuHn41F4aJ
JHIbDvh5OpqwlbdjMDT1JD8V0SqIKBgV0d//E7FySfuA1mW6AcXaf/AZavpG9r45
rwxHC2OVdHfXo5MH7aI9Wax0CFdu2aZOEHLRFQE2KSXKY4/TcRdFpw==
-----END RSA PRIVATE KEY-----
`
	v1 := []byte(`6217690700372158001X000000100TEST11573462713560839b241a0c54022019-11-11 17:01:211.00IACSPAY1911111701171012907901`)

	r, _ := rsa.New(hash.SHA1, rsa.ParsePKCS1PrivateKey([]byte(key)))
	ret, err := r.Sign(v1)
	t.Log(err)

	t.Log("ret:", base64.StdEncoding.EncodeToString(ret))

	assert.True(t, r.Verify(v1, ret))
}
