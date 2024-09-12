package config

type value struct {
}

func (v value) String() string {
	return ""
}

func (v value) Unmarshal(val any) error {
	return nil
}
