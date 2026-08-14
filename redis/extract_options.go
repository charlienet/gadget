package redis

import "github.com/redis/go-redis/v9"

// extractUniversalOptions 从外部 uc 提取其真实连接配置，供 AddPrefix 派生的
// 子连接池使用。go-redis 的 UniversalClient 接口未暴露 Options()，需按具体
// 类型断言：
//
//   - *redis.Client → Options() 返回 *redis.Options
//   - *redis.ClusterClient → Options() 返回 *redis.ClusterOptions
//   - *redis.Ring → 不支持提取（返回 nil，见下方说明）
//
// 返回 nil 表示无法提取（未知类型或 Ring），调用方需通过显式连接 Option
// 提供配置（NewWithClient 会返回错误要求 WithRedisOptions）。
//
// Ring 限制：RingOptions 的 map 地址转地址列表后，NewUniversalClient 见
// 多地址会创建 cluster client——派生子池将以 cluster 协议访问实际非集群的
// shard 节点，功能破坏（比"环配置丢失"更严重）。因此 *redis.Ring 一律不
// 提取，AddPrefix 派生需显式 WithRedisOptions 提供连接配置。
//
// 哨兵限制：go-redis 的哨兵（failover）客户端底层是 *redis.Client，但其
// Options() 返回的 *redis.Options 不含 MasterName（该字段仅存在于
// FailoverOptions/UniversalOptions），故无法从外部 uc 提取哨兵配置。
// 包装外部 failover client 时，AddPrefix 派生子池无法继承哨兵发现能力，
// 需显式传入 WithRedisOptions 提供哨兵配置；本库自建（New/NewWithUrl +
// MasterName）的哨兵 client 的 conf 完整保留 MasterName，不受影响。
func extractUniversalOptions(uc redis.UniversalClient) *redis.UniversalOptions {
	switch v := uc.(type) {
	case *redis.Client:
		return universalOptionsFromClient(v.Options())
	case *redis.ClusterClient:
		return universalOptionsFromCluster(v.Options())
	default:
		// 含 *redis.Ring：不支持提取（见上方 Ring 限制说明）
		return nil
	}
}

// universalOptionsFromClient 将 *redis.Options（单节点）转为 UniversalOptions。
// 注意：go-redis v9.22 的单机 *redis.Options 无 MasterName 字段（哨兵配置仅
// 存在于 FailoverOptions/UniversalOptions），哨兵 client 经此处提取时会丢失
// 哨兵发现配置，见 extractUniversalOptions 的哨兵限制说明。
func universalOptionsFromClient(o *redis.Options) *redis.UniversalOptions {
	return &redis.UniversalOptions{
		Addrs:                        []string{o.Addr},
		ClientName:                   o.ClientName,
		Dialer:                       o.Dialer,
		OnConnect:                    o.OnConnect,
		Protocol:                     o.Protocol,
		Username:                     o.Username,
		Password:                     o.Password,
		CredentialsProvider:          o.CredentialsProvider,
		CredentialsProviderContext:   o.CredentialsProviderContext,
		StreamingCredentialsProvider: o.StreamingCredentialsProvider,
		DB:                           o.DB,
		MaxRetries:                   o.MaxRetries,
		MinRetryBackoff:              o.MinRetryBackoff,
		MaxRetryBackoff:              o.MaxRetryBackoff,
		DialTimeout:                  o.DialTimeout,
		DialerRetries:                o.DialerRetries,
		DialerRetryTimeout:           o.DialerRetryTimeout,
		ReadTimeout:                  o.ReadTimeout,
		WriteTimeout:                 o.WriteTimeout,
		ContextTimeoutEnabled:        o.ContextTimeoutEnabled,
		ReadBufferSize:               o.ReadBufferSize,
		WriteBufferSize:              o.WriteBufferSize,
		PoolFIFO:                     o.PoolFIFO,
		PoolSize:                     o.PoolSize,
		MaxConcurrentDials:           o.MaxConcurrentDials,
		PoolTimeout:                  o.PoolTimeout,
		MinIdleConns:                 o.MinIdleConns,
		MaxIdleConns:                 o.MaxIdleConns,
		MaxActiveConns:               o.MaxActiveConns,
		ConnMaxIdleTime:              o.ConnMaxIdleTime,
		ConnMaxLifetime:              o.ConnMaxLifetime,
		ConnMaxLifetimeJitter:        o.ConnMaxLifetimeJitter,
		TLSConfig:                    o.TLSConfig,
		DisableIdentity:              o.DisableIdentity,
		IdentitySuffix:               o.IdentitySuffix,
		FailingTimeoutSeconds:        o.FailingTimeoutSeconds,
		PushNotificationProcessor:    o.PushNotificationProcessor,
		AutoPipelineOptions:          o.AutoPipelineOptions,
		MaintNotificationsConfig:     o.MaintNotificationsConfig,
		ClientSideCacheConfig:        o.ClientSideCacheConfig,
	}
}

// universalOptionsFromCluster 将 *redis.ClusterOptions（集群）转为 UniversalOptions。
// 注意：ClusterOptions.NewClient（自定义节点客户端工厂）在 UniversalOptions 中
// 无对应字段，无法拷贝，派生池将使用默认节点客户端。
func universalOptionsFromCluster(o *redis.ClusterOptions) *redis.UniversalOptions {
	return &redis.UniversalOptions{
		Addrs:                        o.Addrs,
		ClientName:                   o.ClientName,
		MaxRedirects:                 o.MaxRedirects,
		ReadOnly:                     o.ReadOnly,
		RouteByLatency:               o.RouteByLatency,
		RouteRandomly:                o.RouteRandomly,
		Dialer:                       o.Dialer,
		OnConnect:                    o.OnConnect,
		Protocol:                     o.Protocol,
		Username:                     o.Username,
		Password:                     o.Password,
		CredentialsProvider:          o.CredentialsProvider,
		CredentialsProviderContext:   o.CredentialsProviderContext,
		StreamingCredentialsProvider: o.StreamingCredentialsProvider,
		MaxRetries:                   o.MaxRetries,
		MinRetryBackoff:              o.MinRetryBackoff,
		MaxRetryBackoff:              o.MaxRetryBackoff,
		DialTimeout:                  o.DialTimeout,
		DialerRetries:                o.DialerRetries,
		DialerRetryTimeout:           o.DialerRetryTimeout,
		ReadTimeout:                  o.ReadTimeout,
		WriteTimeout:                 o.WriteTimeout,
		ContextTimeoutEnabled:        o.ContextTimeoutEnabled,
		ReadBufferSize:               o.ReadBufferSize,
		WriteBufferSize:              o.WriteBufferSize,
		PoolFIFO:                     o.PoolFIFO,
		PoolSize:                     o.PoolSize,
		MaxConcurrentDials:           o.MaxConcurrentDials,
		PoolTimeout:                  o.PoolTimeout,
		MinIdleConns:                 o.MinIdleConns,
		MaxIdleConns:                 o.MaxIdleConns,
		MaxActiveConns:               o.MaxActiveConns,
		ConnMaxIdleTime:              o.ConnMaxIdleTime,
		ConnMaxLifetime:              o.ConnMaxLifetime,
		ConnMaxLifetimeJitter:        o.ConnMaxLifetimeJitter,
		TLSConfig:                    o.TLSConfig,
		DisableIdentity:              o.DisableIdentity,
		IdentitySuffix:               o.IdentitySuffix,
		PushNotificationProcessor:    o.PushNotificationProcessor,
		FailingTimeoutSeconds:        o.FailingTimeoutSeconds,
		MaintNotificationsConfig:     o.MaintNotificationsConfig,
	}
}
