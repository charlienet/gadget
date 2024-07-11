多级缓存层

支持本地缓存和远程存储组成多级缓存机制

默认使用本地缓存，可以添加Redis进行分布式存储。

```
c := cache.New(
    WithFreecache(),
    WithRedis(redis.Client))

c.Put(context, "key", v any)
c.Get(context, "key")
c.Delete(context, "key")

```

存储时加载函数

cache.GetFn(context, key, out any, fn LoadFunc, expiration)

判断指定键是否在缓存中

cache.Exist(context, key)

数据同步机制

使用消息订阅和通知机制同步删除缓存内容，在初始化时添加消息队列机制。
cache.WithSubscribe()


数据获取，在缓存中记录空值。
1. 从本地内存缓存获取数据，加载成功后返回。否则下一步
2. 从分布缓存获取数据，记录为空值时调用数据获取函数
3. 数据库不存在，在缓存中记录键值为空
4. 

缓存中数据不存时返回
ErrNotFound         缓存中不存在，需要向加载函数加载数据
ErrEntityNotExist   缓存中缓存有空值，不需要使用加载函数加载数据直接返回数据不存在。