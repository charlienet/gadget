package padding_test

import (
	"testing"

	"github.com/charlienet/gadget/crypto/padding"
	"github.com/charlienet/gadget/misc/bytesconv"
	"github.com/charlienet/gadget/misc/rand"
)

func TestPKCS7(t *testing.T) {
	b, _ := rand.RandBytes(8)
	p := padding.PKCS7.Padding(b, 8)
	t.Log(bytesconv.BytesResult(p).Hex())
	t.Log(bytesconv.BytesResult(padding.PKCS7.UnPadding(p)).Hex())
}

func TestISO10126(t *testing.T) {
	b, _ := rand.RandBytes(8)
	p := padding.ISO10126.Padding(b, 16)
	t.Log(bytesconv.BytesResult(p).Hex())
	t.Log(bytesconv.BytesResult(padding.ISO10126.UnPadding(p)).Hex())
}

func TestANSIX923(t *testing.T) {
	b, _ := rand.RandBytes(3)
	p := padding.ANSIX923.Padding(b, 16)
	t.Log(bytesconv.BytesResult(p).Hex())
	t.Log(bytesconv.BytesResult(padding.ANSIX923.UnPadding(p)).Hex())
}
