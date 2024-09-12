package env

import (
	"os"
	"testing"
)

func TestEnv_Read(t *testing.T) {
	expected := map[string]map[string]string{
		"database": {
			"host":       "localhost",
			"passwrod":   "password",
			"datasource": "user:password@localhost:port/db",
		},
	}

	os.Setenv("DATABASE_HOST", "localhost")
	os.Setenv("DATABASE_PASSWORD", "password")
	os.Setenv("DATABASE_DATASOURCE", "user:password@localhost:port/db")

	s := New()
	err := s.Read()
	if err != nil {
		t.Error(err)
	}

	_ = expected
}
