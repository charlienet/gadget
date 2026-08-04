package redis

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
)

// createMockCmd 创建用于测试的命令对象
func createMockCmd(name string, args ...interface{}) *redis.Cmd {
	allArgs := make([]interface{}, len(args)+1)
	allArgs[0] = name
	copy(allArgs[1:], args)
	
	cmd := redis.NewCmd(context.Background(), allArgs...)
	return cmd
}

func TestRenameHook_ModeA_NoKey(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式A: 无KEY指令 - AUTH, PING, INFO 等
	testCases := [][]interface{}{
		{"AUTH", "password"},
		{"PING"},
		{"INFO"},
		{"BGREWRITEAOF"},
		{"CLIENT", "LIST"},
		{"CLUSTER", "INFO"},
		{"CONFIG", "GET", "timeout"},
		{"DBSIZE"},
		{"ECHO", "hello"},
		{"FLUSHALL"},
		{"FLUSHDB"},
		{"LASTSAVE"},
		{"SAVE"},
		{"TIME"},
		{"FT.SEARCH", "idx", "query"}, // RediSearch commands
		{"TFUNCTION", "LOAD", "code"}, // Functions
	}

	for _, testCase := range testCases {
		cmd := createMockCmd(testCase[0].(string), testCase[1:]...)
		originalArgs := make([]interface{}, len(cmd.Args()))
		copy(originalArgs, cmd.Args())
		
		hook.renameKey(cmd)
		
		// 验证参数没有变化
		assert.Equal(t, originalArgs, cmd.Args(), "Command %s should not have modified arguments", testCase[0])
	}
}

func TestRenameHook_ModeB_ConsecutiveKeys(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式B: 连续KEY - 从args[1]到末尾全部是key
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"DEL", []interface{}{"DEL", "key1", "key2", "key3"}, []interface{}{"DEL", "test:key1", "test:key2", "test:key3"}},
		{"EXISTS", []interface{}{"EXISTS", "key1", "key2"}, []interface{}{"EXISTS", "test:key1", "test:key2"}},
		{"MGET", []interface{}{"MGET", "key1", "key2", "key3"}, []interface{}{"MGET", "test:key1", "test:key2", "test:key3"}},
		{"SDIFF", []interface{}{"SDIFF", "set1", "set2", "set3"}, []interface{}{"SDIFF", "test:set1", "test:set2", "test:set3"}},
		{"SUNIONSTORE", []interface{}{"SUNIONSTORE", "dest", "set1", "set2"}, []interface{}{"SUNIONSTORE", "test:dest", "test:set1", "test:set2"}},
		{"WATCH", []interface{}{"WATCH", "key1", "key2"}, []interface{}{"WATCH", "test:key1", "test:key2"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed all keys", tc.name)
	}
}

func TestRenameHook_ModeC_ExceptLastKey(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式C: 除最后一个外连续KEY - 末尾参数不是key（如timeout）
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"BLPOP", []interface{}{"BLPOP", "key1", "key2", "10"}, []interface{}{"BLPOP", "test:key1", "test:key2", "10"}},
		{"BRPOP", []interface{}{"BRPOP", "key1", "key2", "timeout"}, []interface{}{"BRPOP", "test:key1", "test:key2", "timeout"}},
		{"JSON.MGET", []interface{}{"JSON.MGET", "key1", "key2", "path"}, []interface{}{"JSON.MGET", "test:key1", "test:key2", "path"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed all keys except last", tc.name)
	}
}

func TestRenameHook_ModeD_AlternatingKeys(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式D: 间隔KEY - key/value交替，key在奇数位置(1,3,5...)
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"MSET", []interface{}{"MSET", "key1", "val1", "key2", "val2"}, []interface{}{"MSET", "test:key1", "val1", "test:key2", "val2"}},
		{"MSETNX", []interface{}{"MSETNX", "key1", "val1", "key2", "val2", "key3", "val3"}, []interface{}{"MSETNX", "test:key1", "val1", "test:key2", "val2", "test:key3", "val3"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed alternating keys", tc.name)
	}
}

func TestRenameHook_ModeD2_IntervalKeys(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式D2: key/非key/非key 间隔 - key在1,4,7...
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"JSON.MSET", []interface{}{"JSON.MSET", "key1", "path1", "val1", "key2", "path2", "val2"}, []interface{}{"JSON.MSET", "test:key1", "path1", "val1", "test:key2", "path2", "val2"}},
		{"TS.MADD", []interface{}{"TS.MADD", "key1", "ts1", "val1", "key2", "ts2", "val2"}, []interface{}{"TS.MADD", "test:key1", "ts1", "val1", "test:key2", "ts2", "val2"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed keys at intervals", tc.name)
	}
}

func TestRenameHook_ModeE1_ScriptCommands(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式E1: 脚本类 - 通过args[2]的count指定key数量
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"EVAL", []interface{}{"EVAL", "script", 2, "key1", "key2", "arg1", "arg2"}, []interface{}{"EVAL", "script", 2, "test:key1", "test:key2", "arg1", "arg2"}},
		{"EVALSHA", []interface{}{"EVALSHA", "sha", 1, "key1", "arg1"}, []interface{}{"EVALSHA", "sha", 1, "test:key1", "arg1"}},
		{"FCALL", []interface{}{"FCALL", "func", 2, "key1", "key2", "arg1"}, []interface{}{"FCALL", "func", 2, "test:key1", "test:key2", "arg1"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed script keys", tc.name)
	}
}

func TestRenameHook_ModeE2_AggregateWriteCommands(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式E2: 聚合写命令 - args[1]为dest key，args[2]为count，args[3..3+n]为source keys
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"ZINTERSTORE", []interface{}{"ZINTERSTORE", "dest", 2, "key1", "key2", "WEIGHTS", 1, 2}, []interface{}{"ZINTERSTORE", "test:dest", 2, "test:key1", "test:key2", "WEIGHTS", 1, 2}},
		{"ZUNIONSTORE", []interface{}{"ZUNIONSTORE", "dest", 3, "key1", "key2", "key3"}, []interface{}{"ZUNIONSTORE", "test:dest", 3, "test:key1", "test:key2", "test:key3"}},
		{"CMS.MERGE", []interface{}{"CMS.MERGE", "dest", 2, "key1", "key2"}, []interface{}{"CMS.MERGE", "test:dest", 2, "test:key1", "test:key2"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed aggregate keys", tc.name)
	}
}

func TestRenameHook_ModeE3_ReadOnlyAggregate(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式E3: 只读聚合 - args[1]为count，keys从args[2]开始
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"ZINTER", []interface{}{"ZINTER", 2, "key1", "key2", "WEIGHTS", 1, 2}, []interface{}{"ZINTER", 2, "test:key1", "test:key2", "WEIGHTS", 1, 2}},
		{"ZUNION", []interface{}{"ZUNION", 3, "key1", "key2", "key3"}, []interface{}{"ZUNION", 3, "test:key1", "test:key2", "test:key3"}},
		{"SINTERCARD", []interface{}{"SINTERCARD", 2, "key1", "key2"}, []interface{}{"SINTERCARD", 2, "test:key1", "test:key2"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed readonly aggregate keys", tc.name)
	}
}

func TestRenameHook_ModeF_SubcommandWithKey(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式F: 子命令后有key - args[2]为key
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"OBJECT", []interface{}{"OBJECT", "ENCODING", "key"}, []interface{}{"OBJECT", "ENCODING", "test:key"}},
		{"MEMORY", []interface{}{"MEMORY", "USAGE", "key"}, []interface{}{"MEMORY", "USAGE", "test:key"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed key after subcommand", tc.name)
	}
}

func TestRenameHook_ModeG_FixedTwoKeys(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式G: 固定双 key - args[1]和args[2]都是key
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"COPY", []interface{}{"COPY", "src", "dest", "DB", "1"}, []interface{}{"COPY", "test:src", "test:dest", "DB", "1"}},
		{"LMOVE", []interface{}{"LMOVE", "src", "dest", "LEFT", "RIGHT"}, []interface{}{"LMOVE", "test:src", "test:dest", "LEFT", "RIGHT"}},
		{"BLMOVE", []interface{}{"BLMOVE", "src", "dest", "LEFT", "RIGHT", "10"}, []interface{}{"BLMOVE", "test:src", "test:dest", "LEFT", "RIGHT", "10"}},
		{"TS.CREATERULE", []interface{}{"TS.CREATERULE", "src", "dest", "AGGREGATION", "avg", 1000}, []interface{}{"TS.CREATERULE", "test:src", "test:dest", "AGGREGATION", "avg", 1000}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed both keys", tc.name)
	}
}

func TestRenameHook_ModeH_BITOP(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式H: BITOP - args[2]为dest key，args[3..]为source keys
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"BITOP", []interface{}{"BITOP", "AND", "destkey", "key1", "key2"}, []interface{}{"BITOP", "AND", "test:destkey", "test:key1", "test:key2"}},
		{"BITOP", []interface{}{"BITOP", "OR", "dest", "key1", "key2", "key3"}, []interface{}{"BITOP", "OR", "test:dest", "test:key1", "test:key2", "test:key3"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed all keys", tc.name)
	}
}

func TestRenameHook_ModeI_MIGRATE(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式I: MIGRATE - args[3]为key
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"MIGRATE", []interface{}{"MIGRATE", "host", "port", "key", "0", "1000"}, []interface{}{"MIGRATE", "host", "port", "test:key", "0", "1000"}},
		{"MIGRATE", []interface{}{"MIGRATE", "host", "port", "", "0", "1000", "KEYS", "key1", "key2"}, []interface{}{"MIGRATE", "host", "port", "", "0", "1000", "KEYS", "test:key1", "test:key2"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have renamed migrate key", tc.name)
	}
}

func TestRenameHook_ModeJ_SpecialCommands(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 模式J: 特殊命令
	testCases := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{"XREAD", []interface{}{"XREAD", "STREAMS", "key1", "key2", "id1", "id2"}, []interface{}{"XREAD", "STREAMS", "test:key1", "test:key2", "id1", "id2"}},
		{"XREADGROUP", []interface{}{"XREADGROUP", "GROUP", "group", "consumer", "STREAMS", "key1", "key2", "id1", "id2"}, []interface{}{"XREADGROUP", "GROUP", "group", "consumer", "STREAMS", "test:key1", "test:key2", "id1", "id2"}},
		{"SORT", []interface{}{"SORT", "key", "BY", "pattern", "STORE", "dest"}, []interface{}{"SORT", "test:key", "BY", "pattern", "STORE", "test:dest"}},
		{"GEORADIUS", []interface{}{"GEORADIUS", "key", "lon", "lat", "radius", "unit", "STORE", "dest"}, []interface{}{"GEORADIUS", "test:key", "lon", "lat", "radius", "unit", "STORE", "test:dest"}},
		{"GEORADIUSBYMEMBER", []interface{}{"GEORADIUSBYMEMBER", "key", "member", "radius", "unit", "STOREDIST", "dest"}, []interface{}{"GEORADIUSBYMEMBER", "test:key", "member", "radius", "unit", "STOREDIST", "test:dest"}},
		{"DEBUG", []interface{}{"DEBUG", "OBJECT", "key"}, []interface{}{"DEBUG", "OBJECT", "test:key"}},
	}

	for _, tc := range testCases {
		cmd := createMockCmd(tc.name, tc.input[1:]...) // Skip command name in input
		hook.renameKey(cmd)
		assert.Equal(t, tc.expected, cmd.Args(), "Command %s should have handled special cases correctly", tc.name)
	}
}

func TestRenameHook_Pipeline(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 测试 ProcessPipelineHook 对多个命令的重命名
	cmds := []redis.Cmder{
		createMockCmd("GET", "key1"),
		createMockCmd("SET", "key2", "value"),
		createMockCmd("DEL", "key3", "key4"),
		createMockCmd("MGET", "key5", "key6", "key7"),
	}

	// 验证重命名前的状态
	assert.Equal(t, []interface{}{"GET", "key1"}, cmds[0].Args())
	assert.Equal(t, []interface{}{"SET", "key2", "value"}, cmds[1].Args())
	assert.Equal(t, []interface{}{"DEL", "key3", "key4"}, cmds[2].Args())
	assert.Equal(t, []interface{}{"MGET", "key5", "key6", "key7"}, cmds[3].Args())

	// 执行管道重命名
	err := hook.ProcessPipelineHook(func(ctx context.Context, cmds []redis.Cmder) error {
		return nil
	})(context.Background(), cmds)

	assert.NoError(t, err)
	
	// 验证重命名后的状态
	assert.Equal(t, []interface{}{"GET", "test:key1"}, cmds[0].Args())
	assert.Equal(t, []interface{}{"SET", "test:key2", "value"}, cmds[1].Args())
	assert.Equal(t, []interface{}{"DEL", "test:key3", "test:key4"}, cmds[2].Args())
	assert.Equal(t, []interface{}{"MGET", "test:key5", "test:key6", "test:key7"}, cmds[3].Args())
}

func TestRenameHook_NoPrefix(t *testing.T) {
	// 测试没有前缀的情况，应该不进行任何重命名
	prefix := newPrefix(":", "") // 空前缀
	hook := renameHook{prefix: prefix}

	cmd := createMockCmd("GET", "key1")
	originalArgs := make([]interface{}, len(cmd.Args()))
	copy(originalArgs, cmd.Args())
	
	hook.renameKey(cmd)
	
	// 验证参数没有变化
	assert.Equal(t, originalArgs, cmd.Args(), "Command should not have modified arguments when no prefix is set")
}

func TestCreateSequence(t *testing.T) {
	// 测试 createSepuence 函数
	tests := []struct {
		start, end, step int
		expected         []int
	}{
		{1, 5, 1, []int{1, 2, 3, 4}},
		{1, 6, 2, []int{1, 3, 5}},
		{0, 10, 3, []int{0, 3, 6, 9}},
		{5, 5, 1, []int{}},
		{3, 2, 1, []int{}},
	}

	for _, tc := range tests {
		result := createSepuence(tc.start, tc.end, tc.step)
		assert.Equal(t, tc.expected, result, "createSepuence(%d, %d, %d) should return %v", tc.start, tc.end, tc.step, tc.expected)
	}
}

func TestRenameHook_DefaultMode(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// 测试默认模式：第一个参数为键值
	cmd := createMockCmd("GET", "key1")
	hook.renameKey(cmd)
	assert.Equal(t, []interface{}{"GET", "test:key1"}, cmd.Args(), "Default mode should rename first argument")

	cmd2 := createMockCmd("SET", "key2", "value")
	hook.renameKey(cmd2)
	assert.Equal(t, []interface{}{"SET", "test:key2", "value"}, cmd2.Args(), "Default mode should rename first argument")
}

func TestRenameHook_PubsubNoRename(t *testing.T) {
	prefix := newPrefix(":", "test")
	hook := renameHook{prefix: prefix}

	// PUBSUB 命令不应该重命名参数
	cmd := createMockCmd("PUBSUB", "CHANNELS", "pattern")
	originalArgs := make([]interface{}, len(cmd.Args()))
	copy(originalArgs, cmd.Args())
	
	hook.renameKey(cmd)
	
	// 验证参数没有变化
	assert.Equal(t, originalArgs, cmd.Args(), "PUBSUB command should not have modified arguments")
}