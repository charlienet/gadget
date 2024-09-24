package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHit(t *testing.T) {
	s := newStats()

	s.IncrHit("local")
	s.IncrHit("local")
	s.IncrHit("local")
	s.IncrHit("local")
	s.IncrHit("local")
	s.IncrHit("local")

	s.IncrMiss("local")
	s.IncrMiss("local")
	s.IncrMiss("local")
	s.IncrMiss("local")
	s.IncrMiss("redis")

	assert.Equal(t, uint64(6), s.TotalHits())
	assert.Equal(t, uint64(5), s.TotalMiss())
	assert.Equal(t, uint64(11), s.Total())
}
