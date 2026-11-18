package status

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Service 只提供固定快照；调度管理权限不通过读取令牌传递。
type Service struct {
	mu        sync.RWMutex
	engine    *engine
	quotaMu   sync.Mutex
	nextQuota map[string]time.Time
}

// Reconfigure 先撤销旧授权并收尾采集，配置无效时保持不可用。
func (s *Service) Reconfigure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
	c, key, token, err := loadConfig(configurationPath())
	if err == nil {
		s.engine = newEngine(c, key, token)
		s.engine.client.waitQuota = s.WaitQuota
	}
}

func (s *Service) closeLocked() {
	previous := s.engine
	s.engine = nil
	if previous != nil {
		previous.close()
	}
}

func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

func (s *Service) Handle(raw []byte) any {
	var req managementRequest
	if len(raw) > 65536 || json.Unmarshal(raw, &req) != nil {
		return response(400, map[string]string{"error": "request_invalid"})
	}
	s.mu.RLock()
	current := s.engine
	s.mu.RUnlock()
	if current == nil {
		return response(503, map[string]string{"error": "configuration_unavailable"})
	}
	result := current.handle(req)
	s.mu.RLock()
	unchanged := s.engine == current
	s.mu.RUnlock()
	if !unchanged {
		return response(409, map[string]string{"error": "configuration_changed"})
	}
	return result
}

func Matches(path string) bool { return path == resourcePath }

// WaitQuota 让快照、定时采集和恢复核验共用同一工作区账号的发送间隔。
func (s *Service) WaitQuota(ctx context.Context, account string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.quotaMu.Lock()
		now := time.Now()
		wait := s.nextQuota[account].Sub(now)
		if wait <= 0 {
			if s.nextQuota == nil {
				s.nextQuota = make(map[string]time.Time)
			}
			for k, due := range s.nextQuota {
				if !due.After(now) {
					delete(s.nextQuota, k)
				}
			}
			s.nextQuota[account] = now.Add(time.Minute)
			s.quotaMu.Unlock()
			return nil
		}
		s.quotaMu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
