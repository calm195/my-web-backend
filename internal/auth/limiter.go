package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

type bucket struct {
	Fail, Pending int
	Until         time.Time
}

// Limiter 分别计算账号和 IP 的失败桶，只存 HMAC；预留并发名额以防并行请求越过五次限制。
type Limiter struct {
	mu      sync.Mutex
	key     []byte
	buckets map[string]*bucket
	Now     func() time.Time
}

// NewLimiter 为单实例创建限频桶；密钥只复制到内存，不保存 IP 或原始账号名。
func NewLimiter(key []byte) *Limiter {
	return &Limiter{key: append([]byte(nil), key...), buckets: make(map[string]*bucket), Now: time.Now}
}

// Reserve 返回结算函数；success=true 释放预留名额，失败累积，窗口十五分钟。retry 为至少一秒的剩余等待。
func (l *Limiter) Reserve(username, ip string) (done func(bool), retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	keys := []string{l.digest("ip:" + ip), l.digest("account:" + strings.ToLower(username))}
	for k, b := range l.buckets {
		if !now.Before(b.Until) && b.Pending == 0 {
			delete(l.buckets, k)
		}
	}
	bs := make([]*bucket, 0, 2)
	// 先检查 IP，已被限频的请求不能通过随机用户名持续扩张账号桶；全局上限防止分散来源耗尽内存。
	for _, k := range keys {
		b := l.buckets[k]
		if b == nil {
			if len(l.buckets) >= 10000 {
				return nil, time.Minute
			}
			b = &bucket{Until: now.Add(15 * time.Minute)}
			l.buckets[k] = b
		}
		if b.Fail+b.Pending >= 5 {
			d := b.Until.Sub(now)
			if d < time.Second {
				d = time.Second
			}
			return nil, d
		}
		bs = append(bs, b)
	}
	for _, b := range bs {
		b.Pending++
	}
	var once sync.Once
	return func(success bool) {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			for _, b := range bs {
				b.Pending--
				if !success {
					b.Fail++
				}
			}
		})
	}, 0
}
func (l *Limiter) digest(v string) string {
	h := hmac.New(sha256.New, l.key)
	h.Write([]byte(v))
	return hex.EncodeToString(h.Sum(nil))
}
