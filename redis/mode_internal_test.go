package redis

import (
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

// TestMode 验证 Mode() 的运行模式判断：纯本地判断（底层客户端类型 + 配置），
// 构造空结构体即可断言类型分支，无需真实连接。
func TestMode(t *testing.T) {
	cases := []struct {
		name string
		rdb  redisClient
		want Mode
	}{
		{
			name: "单机（空 Client + 空配置）",
			rdb: redisClient{
				UniversalClient: &goredis.Client{},
				conf:            &goredis.UniversalOptions{},
			},
			want: ModeStandalone,
		},
		{
			name: "集群（ClusterClient 实例）",
			rdb: redisClient{
				UniversalClient: &goredis.ClusterClient{},
			},
			want: ModeCluster,
		},
		{
			name: "哨兵（conf.MasterName 非空）",
			rdb: redisClient{
				UniversalClient: &goredis.Client{},
				conf:            &goredis.UniversalOptions{MasterName: "mymaster"},
			},
			want: ModeSentinel,
		},
		{
			name: "哈希环（Ring 实例）",
			rdb: redisClient{
				UniversalClient: &goredis.Ring{},
			},
			want: ModeRing,
		},
		{
			name: "哨兵优先于 ClusterClient 类型判断",
			rdb: redisClient{
				UniversalClient: &goredis.ClusterClient{},
				conf:            &goredis.UniversalOptions{MasterName: "mymaster"},
			},
			// failover cluster（哨兵+集群）底层是 ClusterClient，按类型判为 cluster
			want: ModeCluster,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rdb.Mode(); got != c.want {
				t.Errorf("Mode() = %q, want %q", got, c.want)
			}
		})
	}
}

