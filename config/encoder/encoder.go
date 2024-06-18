package encoder

type Encoder interface {
	Encode(any) ([]byte, error)
	Decode([]byte, any) error
	String() string
}
