// Package idgen 生成 ULID。
//
// ULID = 48 位毫秒时间戳 + 80 位随机数，用 Crockford Base32 编码成 26 个字符。
// 它按字典序排序等价于按时间排序，且全局唯一，因此适合直接用作对象存储 key 的一部分
// （替代原始文件名，避免中文、空格、特殊符号带来的 URL 编码问题）。
package idgen

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// crockford 是 ULID 使用的 32 字符表，去掉了容易混淆的 I/L/O/U。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu          sync.Mutex
	lastMs      int64
	lastEntropy [10]byte
)

// New 返回一个新的 ULID 字符串。
//
// 同一毫秒内多次调用时，熵部分递增，保证结果严格单调且不重复。
func New() string {
	ms := time.Now().UnixMilli()

	mu.Lock()
	switch {
	case ms == lastMs:
		increment(&lastEntropy)
	case ms > lastMs:
		lastMs = ms
		if _, err := rand.Read(lastEntropy[:]); err != nil {
			// 极端情况下退化用纳秒时间填充，保证不阻塞调用方
			binary.BigEndian.PutUint64(lastEntropy[:8], uint64(time.Now().UnixNano()))
		}
	default:
		// 时钟回拨：沿用上一次的时间戳，只递增熵，保持单调
		increment(&lastEntropy)
		ms = lastMs
	}
	entropy := lastEntropy
	mu.Unlock()

	var buf [16]byte
	buf[0] = byte(ms >> 40)
	buf[1] = byte(ms >> 32)
	buf[2] = byte(ms >> 24)
	buf[3] = byte(ms >> 16)
	buf[4] = byte(ms >> 8)
	buf[5] = byte(ms)
	copy(buf[6:], entropy[:])

	return encode(buf)
}

// increment 对大端序的熵做 +1，溢出则回绕为全零。
func increment(e *[10]byte) {
	for i := len(e) - 1; i >= 0; i-- {
		e[i]++
		if e[i] != 0 {
			return
		}
	}
}

// encode 把 16 字节按 Crockford Base32 编成 26 个字符。
// 26 个字符共 130 位，其中前 2 位恒为 0。
func encode(b [16]byte) string {
	out := make([]byte, 26)
	for i := range out {
		out[i] = crockford[fiveBits(b, i*5)]
	}
	return string(out)
}

// fiveBits 取从第 pos 位（0 为最高位）开始的 5 个比特。
func fiveBits(b [16]byte, pos int) int {
	v := 0
	for j := 0; j < 5; j++ {
		bit := pos + j
		v <<= 1
		if bit < 2 {
			// 前 2 位是补零，跳过
			continue
		}
		idx := bit - 2
		if b[idx/8]&(1<<(7-uint(idx%8))) != 0 {
			v |= 1
		}
	}
	return v
}
