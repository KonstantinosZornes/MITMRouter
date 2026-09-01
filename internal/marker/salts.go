// Marker 动态整数盐值：容量受限的线程安全 LRU。
//
// 设计要点：
//   - 键为 Marker 的 SHA-256 指纹，内存中不保留明文；
//   - 值为自 0 起递增的整数。0 表示"尚无专属盐"（等价于仅系统盐）；
//     上游出口不可用（TLS 错误等）时 +1，使粘滞身份改变从而更换出口 IP；
//   - 容量默认 10000 条：超出即淘汰最久未使用的条目，被淘汰的
//     盐值回落为 0；Get/Rotate 都会刷新其最近使用时间；
//   - 纯内存态，可由调用方选配持久化：启动时以 SeedFingerprint 灌回
//     历史盐值、Rotate 后异步落库；未接持久层时进程重启归零。

package marker

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
)

// DefaultCapacity 默认容量（条目数）。
const DefaultCapacity = 10000

// SaltStore 是身份指纹 → 动态盐值及连续失败计数的 LRU 存储。零值不可用，须经 NewSaltStore 构造。
type SaltStore struct {
	mu  sync.Mutex
	cap int
	c   *lru.Cache[string, saltState]
}

type saltState struct {
	salt     int64
	failures int
}

// NewSaltStore 创建容量为 capacity 的存储；capacity<=0 时取 DefaultCapacity。
func NewSaltStore(capacity int) *SaltStore {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	c, err := lru.New[string, saltState](capacity)
	if err != nil { // 仅在 capacity<=0 时发生，上面已排除
		panic("marker: invalid capacity " + strconv.Itoa(capacity))
	}
	return &SaltStore{cap: capacity, c: c}
}

// keyOf 返回 Marker 的 SHA-256 十六进制指纹（不落明文）。
func keyOf(m string) string { return Fingerprint(m) }

// Fingerprint 返回 Marker 的完整 SHA-256 十六进制指纹，与 LRU 键及
// 持久层 marker_salts.marker_fp 使用同一格式；绝不还原明文。
func Fingerprint(m string) string {
	sum := sha256.Sum256([]byte(m))
	return hex.EncodeToString(sum[:])
}

// Cap 返回 LRU 容量（条目数），供持久化加载时对齐 LIMIT。
func (s *SaltStore) Cap() int { return s.cap }

// Len 返回当前条目数（观测/测试用）。
func (s *SaltStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Len()
}

// SeedFingerprint 将持久层恢复的指纹条目直接灌入 LRU（键已是 Fingerprint 格式，
// 不经过明文）。负值防御性钳为 0；灌入即视为一次使用，参与淘汰排序。
func (s *SaltStore) SeedFingerprint(fp string, salt int64) {
	if salt < 0 {
		salt = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.Add(fp, saltState{salt: salt})
}

// Get 返回身份当前盐值；不存在返回 0 且不插入。
// 命中会刷新该条目的最近使用时间（影响淘汰顺序）。
func (s *SaltStore) Get(m string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.c.Get(keyOf(m)); ok {
		return v.salt
	}
	return 0
}

// Rotate 将盐值 +1（不存在则以 1 初始化）并清空连续失败计数；
// 超出容量时自动淘汰最久未使用条目。返回新盐值。
func (s *SaltStore) Rotate(m string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := keyOf(m)
	v, _ := s.c.Get(k) // 缺失时 Get 不插入，恰好得到初始 0
	v.salt++
	v.failures = 0
	s.c.Add(k, v)
	return v.salt
}

// RecordFailure 记录一次可触发轮换的失败。连续失败达到 threshold 时盐值 +1，
// 并将计数清零；返回是否轮换、新盐值及本次累计次数。threshold<1 按 1 处理。
func (s *SaltStore) RecordFailure(m string, threshold int) (rotated bool, salt int64, failures int) {
	if threshold < 1 {
		threshold = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := keyOf(m)
	v, _ := s.c.Get(k)
	v.failures++
	failures = v.failures
	if v.failures >= threshold {
		v.salt++
		v.failures = 0
		rotated = true
	}
	s.c.Add(k, v)
	return rotated, v.salt, failures
}

// ClearFailures 清除一次成功转发前该身份累计的可轮换失败；不存在不插入。
func (s *SaltStore) ClearFailures(m string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := keyOf(m)
	v, ok := s.c.Get(k)
	if !ok {
		return
	}
	v.failures = 0
	s.c.Add(k, v)
}
