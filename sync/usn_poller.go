package sync

import (
	"context"
	"log"
	"sync"
	"time"
)

// USNQuerier 查詢 USN 變更的介面（由 MongoDB client 實作）
type USNQuerier interface {
	GetLatestUSN(ctx context.Context, userId string) (int, error)
	GetChangesAfterUSN(ctx context.Context, userId string, afterUSN int) ([]SyncEvent, error)
}

// USNPoller 定時輪詢 USN 變更作為 Redis Streams 的兜底
type USNPoller struct {
	querier  USNQuerier
	handler  EventHandler
	interval time.Duration

	mu      sync.RWMutex
	lastUSN map[string]int // userId → lastSyncedUSN

	activeUsersFn func() []string // 取得活躍用戶清單
}

func NewUSNPoller(querier USNQuerier, handler EventHandler, interval time.Duration, activeUsersFn func() []string) *USNPoller {
	return &USNPoller{
		querier:       querier,
		handler:       handler,
		interval:      interval,
		lastUSN:       make(map[string]int),
		activeUsersFn: activeUsersFn,
	}
}

// Start 開始輪詢迴圈
func (p *USNPoller) Start(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollAll(ctx)
		}
	}
}

// SetLastUSN 設定某用戶的最後同步 USN（用於初始化或外部更新）
func (p *USNPoller) SetLastUSN(userId string, usn int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastUSN[userId] = usn
}

// GetLastUSN 取得某用戶的最後同步 USN
func (p *USNPoller) GetLastUSN(userId string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastUSN[userId]
}

func (p *USNPoller) pollAll(ctx context.Context) {
	users := p.activeUsersFn()
	if len(users) == 0 {
		return
	}

	const maxWorkers = 4
	sem := make(chan struct{}, maxWorkers)
	var wg sync.WaitGroup

	for _, userId := range users {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(uid string) {
			defer wg.Done()
			defer func() { <-sem }()
			p.PollUser(ctx, uid)
		}(userId)
	}
	wg.Wait()
}

// PollUser 查詢單一用戶的 USN 變更
func (p *USNPoller) PollUser(ctx context.Context, userId string) int {
	p.mu.RLock()
	lastUSN := p.lastUSN[userId]
	p.mu.RUnlock()

	changes, err := p.querier.GetChangesAfterUSN(ctx, userId, lastUSN)
	if err != nil {
		log.Printf("USN poll error (user=%s): %v", userId, err)
		return 0
	}

	maxUSN := lastUSN
	processed := 0
	for _, event := range changes {
		if err := p.handler.HandleEvent(ctx, event); err != nil {
			log.Printf("USN poll handle error: %v", err)
			continue
		}
		processed++
	}

	if len(changes) > 0 {
		latestUSN, err := p.querier.GetLatestUSN(ctx, userId)
		if err == nil && latestUSN > maxUSN {
			maxUSN = latestUSN
		}
	}

	if maxUSN > lastUSN {
		p.mu.Lock()
		p.lastUSN[userId] = maxUSN
		p.mu.Unlock()
	}

	return processed
}
