# bloom filter 

布隆过滤器，在布隆过滤器中检查某一键不存在则值一定不存在，检查为某一键存在则值不一定存在。


在分布式环境中可添加远程位图存储

1. 本地检查键存在，不用再次检查远程位图。
2. 本地检查键不存在，再次检查远程位图是否存在。
3. 本地位图和远程位图都不存在时表示此键值不存在。
4. 将不存在的键值存入位图

分布式环境中需要同步哈希值