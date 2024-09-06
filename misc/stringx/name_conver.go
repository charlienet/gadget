package stringx

import (
	"slices"
	"strings"

	"github.com/charlienet/gadget/datastructures/maps"
	"github.com/charlienet/gadget/misc/bytesconv"
	"github.com/charlienet/gadget/misc/generics"
)

// CamelCase 		userName
// PascalCase		UserName
// SnakeCase		user_name
// PascalSnakeCase	User_Name
// UpperSnakeCase	USER_NAME

var (
	pascal2snake      = maps.NewHashMap[map[string]string]().Synchronize()
	pascal2upperSnake = maps.NewHashMap[map[string]string]().Synchronize()
	sanke2Pascal      = maps.NewHashMap[map[string]string]().Synchronize()
	sanke2Camel       = maps.NewHashMap[map[string]string]().Synchronize()
)

// Pascal转换为驼峰
func Pascal2Camel(name string) string {
	if len(name) == 0 {
		return name
	}

	data := []byte(name)
	data[0] = toLower(data[0])

	return string(data)
}

// 驼峰转换为Pascal
func Camel2Pascal(name string) string {
	if len(name) == 0 {
		return name
	}

	data := []byte(name)
	data[0] = toUpper(data[0])

	return string(data)
}

func Pascal2Snake(name string) string {
	if len(name) == 0 {
		return name
	}

	if r, ok := pascal2snake.Get(name); ok {
		return r
	}

	names := splitByCapital(name)
	s := strings.Join(names, "_")
	pascal2snake.Set(name, s)

	return s
}

func Pascal2UpperSnake(name string) string {
	if len(name) == 0 {
		return name
	}

	if r, ok := pascal2snake.Get(name); ok {
		return r
	}

	names := splitByCapital(name)

	joined := bytesconv.StringToBytes(strings.Join(names, "_"))
	for i := 0; i < len(joined); i++ {
		joined[i] = toUpper(joined[i])
	}

	s := bytesconv.BytesToString(joined)
	pascal2upperSnake.Set(name, s)

	return s
}

func Camel2UpperSnake(name string) string {
	if len(name) == 0 {
		return name
	}

	return Pascal2UpperSnake(name)
}

func Camel2Snake(name string) string {
	if len(name) == 0 {
		return name
	}

	return Pascal2Snake(name)
}

func Snake2Pascal(name string) string {
	if len(name) == 0 {
		return name
	}

	if s, ok := sanke2Pascal.Get(name); ok {
		return s
	}

	names := strings.Split(name, "_")
	for i, n := range names {
		names[i] = Ucfirst(n)
	}

	s := strings.Join(names, "")
	sanke2Pascal.Set(name, s)
	return s
}

func Snake2Camel(name string) string {
	if len(name) == 0 {
		return name
	}

	if s, ok := sanke2Camel.Get(name); ok {
		return s
	}

	names := strings.Split(name, "_")
	names[0] = Lcfirst(names[0])
	for i := 1; i < len(names); i++ {
		names[i] = Ucfirst(names[i])
	}

	s := strings.Join(names, "")
	sanke2Camel.Set(name, s)

	return s
}

// 首字母大写，其余字母小写
func Ucfirst(str string) string {
	if len(str) == 0 {
		return str
	}

	data := []byte(str)
	data[0] = toUpper(data[0])
	for i := 1; i < len(data); i++ {
		data[i] = toLower(data[i])
	}

	return string(data)
}

// 首字母小写，其余字母小写
func Lcfirst(str string) string {
	data := []byte(str)
	for i := 0; i < len(data); i++ {
		data[i] = toLower(data[i])
	}

	return string(data)
}

func toUpper(c byte) byte {
	if 'a' <= c && c <= 'z' {
		c -= 'a' - 'A'
	}

	return c
}

func toLower(s byte) byte {
	if 'A' <= s && s <= 'Z' {
		s += 'a' - 'A'
	}
	return s
}

var p = generics.NewPool(func() []string {
	return make([]string, 8)
})

func splitByCapital(s string) []string {
	count := countCapital(s) + 1
	if count == 1 {
		return []string{s}
	}

	a := p.Get()
	defer p.Put(a)

	if cap(a) <= count {
		a = slices.Grow(a, count)
	}

	i, n, last := 1, 0, 0
	for ; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			a[n] = s[last:i]

			last = i
			n++
		}
	}

	a[n] = s[last:]
	return a[:n+1]
}

func countCapital(s string) int {
	count := 0
	for i := 1; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			count++
		}
	}
	return count
}
