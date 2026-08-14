package redis

import "github.com/redis/go-redis/v9"

// Mode 表示 Redis 服务的运行模式。
type Mode string

const (
	ModeStandalone Mode = "standalone" // 单机模式
	ModeCluster    Mode = "cluster"    // 集群模式（Redis Cluster / failover cluster）
	ModeSentinel   Mode = "sentinel"   // 哨兵模式
	ModeRing       Mode = "ring"       // 一致性哈希环模式
)

// Mode 返回当前连接的 Redis 服务运行模式。
// 纯本地判断（底层客户端类型 + 连接配置），不发起任何网络调用：
//   - *redis.ClusterClient → ModeCluster（含 Redis Cluster 与 failover cluster）
//   - *redis.Ring → ModeRing（一致性哈希环）
//   - conf.MasterName 非空 → ModeSentinel（哨兵模式，底层实际为 *redis.Client，
//     需靠配置区分；注意 NewWithClient 包装外部 failover client 时，
//     go-redis 的 Options() 不保留 MasterName，可能回落为 ModeStandalone）
//   - 其余 → ModeStandalone
func (rdb redisClient) Mode() Mode {
	switch rdb.UniversalClient.(type) {
	case *redis.ClusterClient:
		return ModeCluster
	case *redis.Ring:
		return ModeRing
	}
	if rdb.conf != nil && rdb.conf.MasterName != "" {
		return ModeSentinel
	}
	return ModeStandalone
}
