package stringx

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCamel2Pascal(t *testing.T) {
	expected := []struct {
		actual string
		expect string
	}{
		{"updatedAt", "UpdatedAt"},
		{"name", "Name"},
		{"createdAt", "CreatedAt"},
		{"n", "N"},
		{"", ""},
	}

	for _, n := range expected {
		assert.Equal(t, n.expect, Camel2Pascal(n.actual))
	}
}

func TestPascal2Camel(t *testing.T) {
	expected := []struct {
		actual string
		expect string
	}{
		{"UpdatedAt", "updatedAt"},
		{"Name", "name"},
		{"CreatedAt", "createdAt"},
		{"N", "n"},
		{"", ""},
	}

	for _, n := range expected {
		assert.Equal(t, n.expect, Pascal2Camel(n.actual))
	}
}

func TestSnake2Pascal(t *testing.T) {
	expected := []struct {
		actual string
		expect string
	}{
		{"UPDATED_AT", "UpdatedAt"},
		{"Name", "Name"},
		{"created_at", "CreatedAt"},
		{"N", "N"},
		{"", ""},
	}

	for _, n := range expected {
		assert.Equal(t, n.expect, Snake2Pascal(n.actual))
	}
}

func TestSnake2Camel(t *testing.T) {
	expected := []struct {
		actual string
		expect string
	}{
		{"UPDATED_AT", "updatedAt"},
		{"Name", "name"},
		{"created_at", "createdAt"},
		{"N", "n"},
		{"", ""},
	}

	for _, n := range expected {
		assert.Equal(t, n.expect, Snake2Camel(n.actual))
	}
}

func TestPascal2Snake(t *testing.T) {
	expected := []struct {
		actual string
		expect string
	}{
		{"UpdatedAt", "Updated_At"},
		{"Name", "Name"},
		{"createdAt", "created_At"},
		{"N", "N"},
		{"", ""},
	}

	for _, n := range expected {
		assert.Equal(t, n.expect, Pascal2Snake(n.actual))
	}
}

func TestPascal2UpperSnake(t *testing.T) {
	expected := []struct {
		actual string
		expect string
	}{
		{"UpdatedAt", "UPDATED_AT"},
		{"Name", "NAME"},
		{"createdAt", "CREATED_AT"},
		{"N", "N"},
		{"", ""},
	}

	for _, n := range expected {
		assert.Equal(t, n.expect, Pascal2UpperSnake(n.actual))
	}
}

func TestCamel2UpperSnake(t *testing.T) {
	expected := []struct {
		actual string
		expect string
	}{
		{"updatedAt", "UPDATED_AT"},
		{"name", "NAME"},
		{"createdAt", "CREATED_AT"},
		{"n", "N"},
		{"", ""},
	}

	for _, n := range expected {
		assert.Equal(t, n.expect, Camel2UpperSnake(n.actual))
	}
}

func TestCamel2Snake(t *testing.T) {
	expected := []struct {
		actual string
		expect string
	}{
		{"updatedAt", "updated_At"},
		{"name", "name"},
		{"createdAt", "created_At"},
		{"n", "n"},
		{"", ""},
	}

	for _, n := range expected {
		assert.Equal(t, n.expect, Camel2Snake(n.actual))
	}
}

func TestUpper(t *testing.T) {
	expected := []struct {
		actual byte
		expect byte
	}{
		{'A', 'A'},
		{'Z', 'Z'},
		{'a', 'A'},
		{'z', 'Z'},
	}

	for _, n := range expected {
		assert.Equal(t, n.expect, toUpper(n.actual))
	}
}

func BenchmarkTransform(b *testing.B) {
	b.Run("Pascal2Camel", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			Pascal2Camel("UpdatedAt")
		}
	})

	b.Run("Camel2Pascal", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			Camel2Pascal("updatedAt")
		}
	})

	b.Run("Snake2Pascal", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			Snake2Pascal("UPDATED_AT")
		}
	})

	b.Run("Snake2Camel", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			Snake2Camel("UPDATED_AT")
		}

	})

	b.Run("Pascal2Snake", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			Pascal2Snake("UpdatedAt")
		}
	})

	b.Run("Pascal2UpperSnake", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			Pascal2UpperSnake("UpdatedAt")
		}
	})

	b.Run("Camel2Snake", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			Camel2Snake("updatedAt")
		}
	})

	b.Run("Camel2UpperSnake", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			Camel2UpperSnake("updatedAt")
		}
	})
}

func TestSplitByCapital2(t *testing.T) {
	output(splitByCapital("abc"))
	output(splitByCapital("UpdatedAt"))
	output(splitByCapital("UpdatedByDayTuesday"))
	output(splitByCapital("Updated"))
	output(splitByCapital(""))
}

func output(s []string) {
	for _, v := range s {
		print(string(v) + " ")
	}
	println()
}

func TestCount(t *testing.T) {
	t.Log(countCapital(("UpdatedByDayTuesday")))
	t.Log(countCapital(("Name")))
	t.Log(countCapital(("Name")))
	t.Log(countCapital(("name")))
}

func BenchmarkCount(b *testing.B) {
	data := "Updated_By_Day_Tuesday"

	b.Run("sys", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			strings.Count(data, "_")
		}
	})

	b.Run("generic_cap", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			countCapital(data)
		}
	})

}

func BenchmarkSplit(b *testing.B) {
	b.Run("splitByCapital", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			splitByCapital("UpdatedByDayTuesday")
		}
	})
}

func BenchmarkUcfirst(b *testing.B) {
	b.Run("Ucfirst", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			Ucfirst("UCFIRST")
		}
	})
}
