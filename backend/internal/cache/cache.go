// Package cache 封装平台对 Redis 的使用。
//
// 平台在 Redis 上存四类东西，本文件负责前三类（第四类「日志流」见 logstream_redis.go）：
//
//	会话键    yn:session:{uid}:{jti}     登出/改权限后主动失效
//	登录失败  yn:loginfail:{username}    账号维度限流，防暴力破解
//	部署互斥  yn:lock:deploy:{env}:{svc} 同一环境同一服务不允许并发部署
//
// 设计取舍：Redis 是**可选**的。enabled=false 时所有方法退化为「无副作用 + 找不到」，
// 平台仍能靠进程内实现完整运行（单实例部署）。这样本地开发、单机交付都不依赖 Redis，
// 而多实例部署时打开开关即可获得跨实例语义。
package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/2777484478/yunnian/internal/config"
)

// releaseScript 释放锁：只有持有者本人（token 匹配）才能删。
// 这是分布式锁的经典易错点——若不校验 token，A 的锁超时后被 B 拿到，
// A 执行完再删一次就会把 B 的锁删掉，互斥随即失效。
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

// Cache Redis 访问入口。零值不可用，必须经 New 构造。
type Cache struct {
	rdb       *redis.Client
	prefix    string
	on        bool
	streamTTL time.Duration
}

// New 按配置构造。enabled=false 时返回一个所有方法都安全的空实现。
func New(ctx context.Context, cfg config.RedisConfig) (*Cache, error) {
	c := &Cache{prefix: cfg.Prefix, streamTTL: cfg.LogStreamTTL.D()}
	if c.prefix == "" {
		c.prefix = "yn:"
	}
	if c.streamTTL <= 0 {
		c.streamTTL = 24 * time.Hour
	}
	if !cfg.Enabled {
		return c, nil
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Username:     cfg.Username,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		PoolSize:     16,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("连接 Redis(%s) 失败: %w", cfg.Addr, err)
	}
	c.rdb = rdb
	c.on = true
	return c, nil
}

// Enabled 是否真正连上了 Redis。
func (c *Cache) Enabled() bool { return c != nil && c.on }

// Close 释放连接。
func (c *Cache) Close() error {
	if !c.Enabled() {
		return nil
	}
	return c.rdb.Close()
}

// Ping 连通性探测，供 /readyz 使用。
func (c *Cache) Ping(ctx context.Context) error {
	if !c.Enabled() {
		return errors.New("redis 未启用")
	}
	return c.rdb.Ping(ctx).Err()
}

// key 拼上统一前缀。所有键都必须经过这里，避免误删共享实例上别人的键。
func (c *Cache) key(parts ...string) string {
	out := c.prefix
	for i, p := range parts {
		if i > 0 {
			out += ":"
		}
		out += p
	}
	return out
}

// --------------------------------------------------------------------------- 会话

// sessionKey 会话键。带上令牌 ID（jti）：同一用户多端登录时各自独立，
// 登出某一端不会误伤另一端的会话。
func (c *Cache) sessionKey(userID uint64, tokenID string) string {
	return c.key("session", strconv.FormatUint(userID, 10), tokenID)
}

// SaveSession 记录一次登录。value 存角色码，便于排查"这个人当时是什么角色"。
func (c *Cache) SaveSession(ctx context.Context, userID uint64, tokenID, roleCode string, ttl time.Duration) error {
	if !c.Enabled() || tokenID == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return c.rdb.Set(ctx, c.sessionKey(userID, tokenID), roleCode, ttl).Err()
}

// SessionValid 判断会话是否仍有效。
//
// 注意与 JWT 的关系：令牌签名有效 ≠ 会话有效。JWT 无法主动吊销，
// 所以这里做一次"双校验"——管理员禁用某用户后，即使他手里的令牌还没过期，
// 也会因为会话键被清掉而立即失效。
//
// Redis 未启用时恒返回 true（退化为纯 JWT 校验，单实例开发场景可接受）。
func (c *Cache) SessionValid(ctx context.Context, userID uint64, tokenID string) bool {
	if !c.Enabled() || tokenID == "" {
		return true
	}
	n, err := c.rdb.Exists(ctx, c.sessionKey(userID, tokenID)).Result()
	return err != nil || n > 0
}

// DropSession 吊销单个会话（登出）。
func (c *Cache) DropSession(ctx context.Context, userID uint64, tokenID string) error {
	if !c.Enabled() || tokenID == "" {
		return nil
	}
	return c.rdb.Del(ctx, c.sessionKey(userID, tokenID)).Err()
}

// DropUserSessions 吊销某用户的全部会话（禁用账号、改角色、重置密码）。
// 用 SCAN 而不是 KEYS：KEYS 在大 keyspace 上会阻塞 Redis 主线程。
func (c *Cache) DropUserSessions(ctx context.Context, userID uint64) (int64, error) {
	if !c.Enabled() {
		return 0, nil
	}
	pattern := c.key("session", strconv.FormatUint(userID, 10), "*")
	var (
		cursor uint64
		total  int64
	)
	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			return total, err
		}
		if len(keys) > 0 {
			n, err := c.rdb.Del(ctx, keys...).Result()
			if err != nil {
				return total, err
			}
			total += n
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return total, nil
}

// --------------------------------------------------------------------------- 登录失败限流

// LoginFailKey 返回限流键名，供诊断与测试使用。
func (c *Cache) LoginFailKey(username string) string {
	return c.key("loginfail", username)
}

// IncLoginFail 记一次失败并返回当前窗口内失败次数。
//
// 用 INCR + 首次设置过期：这样窗口是「首次失败起算的滑动窗口」，
// 而不是「固定时间片」——后者会出现"跨片瞬间连试 10 次"的绕过。
func (c *Cache) IncLoginFail(ctx context.Context, username string, window time.Duration) (int64, error) {
	if !c.Enabled() {
		return memLimiter.inc(username, window), nil
	}
	k := c.LoginFailKey(username)
	n, err := c.rdb.Incr(ctx, k).Result()
	if err != nil {
		return 0, err
	}
	if n == 1 {
		if err := c.rdb.Expire(ctx, k, window).Err(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// LoginFailCount 读取当前失败次数。
func (c *Cache) LoginFailCount(ctx context.Context, username string) int64 {
	if !c.Enabled() {
		return memLimiter.count(username)
	}
	n, err := c.rdb.Get(ctx, c.LoginFailKey(username)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	return n
}

// ResetLoginFail 登录成功后清零。
func (c *Cache) ResetLoginFail(ctx context.Context, username string) error {
	if !c.Enabled() {
		memLimiter.reset(username)
		return nil
	}
	return c.rdb.Del(ctx, c.LoginFailKey(username)).Err()
}

// --------------------------------------------------------------------------- 分布式锁

// TryLock 尝试加锁。语义与 queue.Locker 一致：拿不到立即返回 false，不阻塞等待。
func (c *Cache) TryLock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	if !c.Enabled() {
		return memLocker.tryLock(key, ttl)
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	token := randomHex(16)
	ok, err := c.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			// 用后台 context：请求 context 可能已经取消，但锁必须释放。
			// 否则任务失败/超时后锁要等到 TTL 才过期，人为造成长时间不可发布。
			_ = releaseScript.Run(context.WithoutCancel(ctx), c.rdb, []string{key}, token).Err()
		})
	}
	return release, true, nil
}

// Name 实现名。
func (c *Cache) Name() string {
	if c.Enabled() {
		return "redis"
	}
	return "memory"
}

// --------------------------------------------------------------------------- 通用

// Get 读取字符串，不存在返回 ("", false)。
func (c *Cache) Get(ctx context.Context, key string) (string, bool) {
	if !c.Enabled() {
		return "", false
	}
	v, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return "", false
	}
	return v, true
}

// Set 写入字符串并设过期。
func (c *Cache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if !c.Enabled() {
		return nil
	}
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

// Del 删除若干键。
func (c *Cache) Del(ctx context.Context, keys ...string) error {
	if !c.Enabled() || len(keys) == 0 {
		return nil
	}
	return c.rdb.Del(ctx, keys...).Err()
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

// --------------------------------------------------------------------------- 无 Redis 时的兜底
//
// 这两份进程内实现只在 redis.enabled=false 时启用。它们与 Redis 版语义一致，
// 只是作用域限于单进程——单实例部署下没有区别。

type memLimiterStore struct {
	mu    sync.Mutex
	items map[string]memLimitItem
}

type memLimitItem struct {
	count   int64
	expires time.Time
}

var memLimiter = &memLimiterStore{items: map[string]memLimitItem{}}

func (m *memLimiterStore) inc(username string, window time.Duration) int64 {
	if window <= 0 {
		window = 5 * time.Minute
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.items[username]
	if it.expires.Before(now) {
		it = memLimitItem{expires: now.Add(window)}
	}
	it.count++
	m.items[username] = it
	return it.count
}

func (m *memLimiterStore) count(username string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := m.items[username]
	if it.expires.Before(time.Now()) {
		return 0
	}
	return it.count
}

func (m *memLimiterStore) reset(username string) {
	m.mu.Lock()
	delete(m.items, username)
	m.mu.Unlock()
}

// memLockStore 进程内锁，带 token 校验。
type memLockStore struct {
	mu    sync.Mutex
	locks map[string]memLockItem
}

type memLockItem struct {
	token   string
	expires time.Time
}

var memLocker = &memLockStore{locks: map[string]memLockItem{}}

func (m *memLockStore) tryLock(key string, ttl time.Duration) (func(), bool, error) {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	token := randomHex(16)
	now := time.Now()

	m.mu.Lock()
	if it, held := m.locks[key]; held && it.expires.After(now) {
		m.mu.Unlock()
		return nil, false, nil
	}
	m.locks[key] = memLockItem{token: token, expires: now.Add(ttl)}
	m.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			// 只删自己那一把：如果锁已过期并被别人重新拿到，这里不能动它。
			if it, ok := m.locks[key]; ok && it.token == token {
				delete(m.locks, key)
			}
		})
	}
	return release, true, nil
}
