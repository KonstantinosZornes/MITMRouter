// Package sticky 实现从唯一标识(Marker)到粘滞身份(account)的无状态推导。
// 纯函数、零共享状态——进程重启/多实例天然一致。
package sticky

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Derive 按以下公式推导粘滞身份：account = lowercase_hex( SHA256(salt ‖ marker) )[:sidLen]
func Derive(salt, marker string, sidLen int) string {
	if sidLen <= 0 {
		sidLen = 16
	}
	if sidLen > 64 {
		sidLen = 64
	}
	sum := sha256.Sum256([]byte(salt + marker))
	return hex.EncodeToString(sum[:])[:sidLen]
}

// CombineSalt 把系统盐与 Marker 动态整数盐合成单一盐串。
// 动态盐为 0（未轮换过）时原样返回系统盐，保持与历史推导结果完全一致；
// 一旦轮换即追加 "#k<n>" 后缀，确保新旧身份必然不同。
func CombineSalt(systemSalt string, perMarker int64) string {
	if perMarker == 0 {
		return systemSalt
	}
	return systemSalt + "#k" + strconv.FormatInt(perMarker, 10)
}

// Fingerprint 返回 sha256(Marker)[:8]，仅用于日志展示（绝不落明文）。
func Fingerprint(marker string) string {
	sum := sha256.Sum256([]byte(marker))
	return hex.EncodeToString(sum[:])[:8]
}
