redis 

键前缀

使用redis的hook机制完成统一的键前缀添加，使用指定的分隔符对键前缀进行分割


约束
rdb:=redis.New()
rdb.Constraint(Ping())
