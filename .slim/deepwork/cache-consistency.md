# Cache 一致性增强方案

## 三个缺失能力

### 1. 版本号校验（CAS 式一致性）

**目标**：本地缓存的 key 能从版本维度确认远端数据是否更新。

**实现方案**：
- Redis 存储层扩展：每条缓存 key 附带一个 version key（`k:ver`），存储 unix 毫秒时间戳
- 本地 `item` 结构扩展：添加 `version` 字段
- `getFromStore(remote)` 时：同时获取数据和版本
- `putInStore(remote)` 时：更新数据后更新版本 key
- **新增**：后台 `versionSyncLoop` goroutine

```
versionSyncLoop:
  每 interval 遍历本地活跃 key（mem_store 支持列举 key）
  对每个 key: GET k:ver → 与本地 version 比较
    ├─ remote version > local → 拉取新数据 → 更新本地
    ├─ remote key 不存在       → 清理本地
    └─ remote version == local → 跳过
```

**遍历性能问题**：所有本地 key 逐一检查在大量 key 时不现实
**优化方案**：每次只检查一个随机子集（采样比例可配），或者检查最近 N 个写入的 key

### 2. Stream 可靠失效通知

**目标**：替代 PubSub 的最多一次送达，提供至少一次语义。

**现有 PubSub Listener**（`plugins/cache/listener/redis/`）：
- 基于 `SUBSCRIBE/PUBLISH`
- 网络闪断时消息丢失，不重试

**新建 Stream Listener**（`plugins/cache/listener/stream/`）：

```
publish(key):
  XADD cache:invalidate * key <key>

watch():
  XREADGROUP GROUP cache-group consumer BLOCK 0
    STREAMS cache:invalidate >
  收到 → 处理 → XACK
  失败 → 不 ACK → 重试
```

复用 `cache.Listener` 接口。

### 3. 冷 key 一致性

**问题**：概率性校验 `WithVerifyEvery(N)` 依赖访问频率驱动。冷 key 长时间不被访问，过期后仍被认为有效。

**解决方案**：版本同步 goroutine 天然覆盖此问题（定期检查所有活跃 key）。

核心公式：一致性延迟 ≤ max(verifyCheckInterval, versionSyncInterval)

---

## 实施路线

| Phase | 功能 | 主要文件 | 工作量 |
|-------|------|----------|--------|
| P0 | 版本号支持（Store + 序列化） | `memory_store.go`, `cache.go`, `plugins/cache/redis/redis.go` | 中 |
| P0 | 版本同步 goroutine | `cache.go` | 中 |
| P1 | Stream 可靠监听器 | NEW `plugins/cache/listener/stream/` | 大 |
| P1 | 冷 key 采样 SCAN | `cache.go`, `memory_store.go` (新增 KeyLister) | 小 |
