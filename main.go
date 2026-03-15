package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/urusaqqrun/vault-mirror-service/api"
	"github.com/urusaqqrun/vault-mirror-service/config"
	"github.com/urusaqqrun/vault-mirror-service/database"
	"github.com/urusaqqrun/vault-mirror-service/executor"
	"github.com/urusaqqrun/vault-mirror-service/mirror"
	"github.com/urusaqqrun/vault-mirror-service/model"
	vaultsync "github.com/urusaqqrun/vault-mirror-service/sync"
)

func main() {
	cfg := config.Load()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Printf("vault-mirror-service 啟動中 (port=%s, vault=%s)", cfg.Port, cfg.VaultRoot)

	// Redis
	rdb := initRedis(cfg.RedisURI)
	defer rdb.Close()

	// PostgreSQL
	pgStore, err := database.NewPgStore(ctx, cfg.PostgresURI)
	if err != nil {
		log.Fatalf("PostgreSQL 連線失敗: %v", err)
	}
	defer pgStore.Close()
	pgStore.SetRedis(rdb)

	// DataReader（由 PgStore 實作）
	var dataReader vaultsync.DataReader = pgStore

	// VaultFS（EFS 實作）
	vaultFS := &mirror.RealVaultFS{Root: cfg.VaultRoot}

	// Claude CLI executor
	claudeExec := executor.NewClaudeExecutor(
		cfg.MaxConcurrentTasks,
		time.Duration(cfg.TaskTimeoutMinutes)*time.Minute,
		cfg.VaultRoot,
	)

	// Vault Lock（優先使用 Redis 分散式鎖，失敗時退回本機鎖）
	var vaultLock executor.VaultLocker = executor.NewVaultLock()
	if rdb != nil {
		lockTTL := time.Duration(cfg.TaskTimeoutMinutes+5) * time.Minute
		vaultLock = executor.NewRedisVaultLock(rdb, lockTTL)
	}

	// Task executor（整合 Claude CLI + VaultFS + Vault Lock + PG 回寫 + 衝突判定 + 版本號遞增）
	taskExec := &fullTaskExecutor{
		claudeExec:   claudeExec,
		vaultLock:    vaultLock,
		vaultFS:      vaultFS,
		vaultRoot:    cfg.VaultRoot,
		writer:       pgStore,
		usnReader:    pgStore,
		usnInc:       pgStore,
		usnSyncer:    pgStore,
		latestUSN:    pgStore,
		latestSeq:    pgStore,
		fullExporter: pgStore,
		ctx:          ctx,
	}
	taskStore := selectTaskStore(api.NewMemoryTaskStore(), rdb, cfg.TaskTimeoutMinutes)

	// API server
	taskHandler := api.NewTaskHandler(taskExec, taskStore)
	taskHandler.SetContext(ctx)
	mux := http.NewServeMux()
	taskHandler.RegisterRoutes(mux)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	server := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: mux,
	}

	g, gCtx := errgroup.WithContext(ctx)

	// Redis Streams consumer
	eventHandler := vaultsync.NewSyncEventHandler(vaultFS, dataReader)
	eventHandler.SetLocker(vaultLock)
	eventHandler.StartCacheEvictor(ctx)
	consumerName := "mirror-" + getHostname()
	consumer := vaultsync.NewConsumer(rdb, eventHandler, consumerName)
	log.Printf("Redis consumer name: %s", consumerName)
	g.Go(func() error {
		if err := consumer.Start(gCtx); err != nil && gCtx.Err() == nil {
			return fmt.Errorf("consumer: %w", err)
		}
		return nil
	})

	// USN poller 兜底（使用 PgStore 的 sync_changes 查詢）
	activeUsersFn := func() []string {
		c, cancelFn := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancelFn()
		users, err := pgStore.ListActiveUsers(c)
		if err != nil {
			log.Printf("list active users failed: %v", err)
			return []string{}
		}
		return users
	}
	poller := vaultsync.NewUSNPoller(
		pgStore,
		eventHandler,
		time.Duration(cfg.USNPollIntervalSec)*time.Second,
		activeUsersFn,
	)
	taskExec.poller = poller
	g.Go(func() error {
		poller.Start(gCtx)
		return nil
	})

	// HTTP server
	g.Go(func() error {
		log.Printf("HTTP server listening on :%s", cfg.Port)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			return fmt.Errorf("http: %w", err)
		}
		return nil
	})

	// 收到中斷信號時觸發 shutdown
	g.Go(func() error {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		select {
		case sig := <-quit:
			fmt.Printf("\n收到信號 %v，正在關閉...\n", sig)
		case <-gCtx.Done():
		}
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		return server.Shutdown(shutdownCtx)
	})

	if err := g.Wait(); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Println("vault-mirror-service 已停止")
}

func initRedis(uri string) *redis.Client {
	opt, err := redis.ParseURL(uri)
	if err != nil {
		log.Printf("Redis URI 解析失敗，使用預設: %v", err)
		opt = &redis.Options{Addr: uri}
	}
	opt.PoolSize = 15
	opt.MinIdleConns = 3

	rdb := redis.NewClient(opt)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Printf("Redis 連線失敗（非致命）: %v", err)
	} else {
		log.Println("Redis 連線成功")
	}
	return rdb
}

// latestUSNReader 查詢用戶最大版本號（衝突判定起始基準）
type latestUSNReader interface {
	GetLatestUSN(ctx context.Context, userID string) (int, error)
}

// seqReader 查詢 sync_changes 最大 seq（Poller 游標推進用）
type seqReader interface {
	GetLatestSeq(ctx context.Context, userID string) (int, error)
}

// fullTaskExecutor 整合所有執行元件
type fullTaskExecutor struct {
	claudeExec   *executor.ClaudeExecutor
	vaultLock    executor.VaultLocker
	vaultFS      mirror.VaultFS
	vaultRoot    string
	writer       executor.DataWriter
	usnReader    executor.USNReader
	usnInc       executor.USNIncrementer
	usnSyncer    executor.USNSyncer
	latestUSN    latestUSNReader
	latestSeq    seqReader
	fullExporter vaultsync.FullExporter
	poller       *vaultsync.USNPoller
	ctx          context.Context
}

func (e *fullTaskExecutor) Execute(task *api.Task) error {
	workDir := fmt.Sprintf("%s/%s", e.vaultRoot, task.UserID)

	if !e.vaultLock.Lock(task.UserID, task.ID) {
		return fmt.Errorf("user vault is locked by another task")
	}
	defer e.vaultLock.Unlock(task.UserID, task.ID)

	e.vaultFS.MkdirAll(task.UserID)

	execCtx := e.ctx
	if execCtx == nil {
		execCtx = context.Background()
	}

	// 確保 Vault 已初始化（CLAUDE.md 不存在時做全量匯出）
	if e.fullExporter != nil && !e.vaultFS.Exists(task.UserID+"/CLAUDE.md") {
		if err := vaultsync.ExportFullVault(execCtx, e.vaultFS, e.fullExporter, task.UserID); err != nil {
			log.Printf("[Task %s] full export error: %v", task.ID, err)
		}
	}

	// 1. 記錄 AI 開始前的最大版本號（衝突判定用）
	aiStartUSN := 0
	if e.latestUSN != nil {
		if usn, usnErr := e.latestUSN.GetLatestUSN(execCtx, task.UserID); usnErr == nil {
			aiStartUSN = usn
		}
	}

	// 2. 執行前快照 + path→ID 映射（刪除回寫用）
	beforeSnap, beforeIDMap, snapErr := executor.TakeSnapshotAndPathIDMap(e.vaultFS, task.UserID)
	if snapErr != nil {
		return fmt.Errorf("snapshot before error: %w", snapErr)
	}

	// 3. 執行 Claude CLI
	output, err := e.claudeExec.ExecuteTask(execCtx, task.ID, workDir, task.Instruction)
	if err != nil {
		return err
	}
	task.Result = output

	// 4. 執行後快照 + diff
	afterSnap, snapErr := executor.TakeSnapshot(e.vaultFS, task.UserID)
	if snapErr != nil {
		return fmt.Errorf("snapshot after error: %w", snapErr)
	}

	diff := executor.ComputeDiff(beforeSnap, afterSnap)
	hasChanges := len(diff.Created) > 0 || len(diff.Modified) > 0 || len(diff.Deleted) > 0 || len(diff.Moved) > 0
	if !hasChanges {
		log.Printf("[Task %s] no vault changes detected", task.ID)
		return nil
	}

	log.Printf("[Task %s] diff: +%d ~%d -%d mv%d",
		task.ID, len(diff.Created), len(diff.Modified), len(diff.Deleted), len(diff.Moved))

	// 5. 解析變更
	importer := mirror.NewImporter(e.vaultFS)
	movedEntries := make([]mirror.MovedFileEntry, len(diff.Moved))
	for i, m := range diff.Moved {
		movedEntries[i] = mirror.MovedFileEntry{OldPath: m.OldPath, NewPath: m.NewPath}
	}
	entries, parseErr := importer.ProcessDiff(task.UserID, diff.Created, diff.Modified, diff.Deleted, movedEntries, beforeIDMap)
	if parseErr != nil {
		return fmt.Errorf("parse diff error: %w", parseErr)
	}

	// 6. 回寫 PostgreSQL（含版本號衝突判定 + 版本號遞增 + sync_changes）
	if e.writer != nil && len(entries) > 0 {
		result := executor.WriteBack(execCtx, e.writer, e.usnReader, e.usnInc, task.UserID, entries, aiStartUSN)
		log.Printf("[Task %s] writeback: created=%d updated=%d moved=%d deleted=%d skipped=%d errors=%d",
			task.ID, result.Created, result.Updated, result.Moved, result.Deleted, result.Skipped, result.Errors)

		totalOps := result.Created + result.Updated + result.Moved + result.Deleted
		if result.Errors > 0 && totalOps == 0 {
			return fmt.Errorf("writeback failed: all %d entries had errors", result.Errors)
		}

		// PG 模式下 SyncUserUSN 為 no-op，但保留呼叫以維持介面一致
		if e.usnSyncer != nil {
			if syncErr := e.usnSyncer.SyncUserUSN(execCtx, task.UserID); syncErr != nil {
				log.Printf("[Task %s] SyncUserUSN error: %v", task.ID, syncErr)
			}
		}

		// 推進 USN Poller 的游標，避免回寫的資料被 poller 重複導出
		// 使用 sync_changes.seq（Poller 實際追蹤的游標）而非 base_items.version
		if e.poller != nil && e.latestSeq != nil {
			if seq, err := e.latestSeq.GetLatestSeq(execCtx, task.UserID); err == nil {
				e.poller.SetLastUSN(task.UserID, seq)
			}
		}
	}

	return nil
}

func (e *fullTaskExecutor) Cancel(taskID string) error {
	return e.claudeExec.Cancel(taskID)
}

func selectTaskStore(fallback api.TaskStore, rdb *redis.Client, taskTimeoutMinutes int) api.TaskStore {
	if rdb != nil {
		if err := rdb.Ping(context.Background()).Err(); err == nil {
			return api.NewRedisTaskStore(rdb, taskTimeoutMinutes)
		}
		log.Printf("Redis 不可用，TaskStore 降級 MemoryStore")
	}
	return fallback
}

// noopDataReader 用於降級模式。
type noopDataReader struct{}

func (n *noopDataReader) ListFolders(ctx context.Context, userID string) ([]*model.Folder, error) {
	return []*model.Folder{}, nil
}
func (n *noopDataReader) GetFolder(ctx context.Context, userID, folderID string) (*model.Folder, error) {
	return nil, nil
}
func (n *noopDataReader) GetNote(ctx context.Context, userID, noteID string) (*model.Note, error) {
	return nil, nil
}
func (n *noopDataReader) GetCard(ctx context.Context, userID, cardID string) (*model.Card, error) {
	return nil, nil
}
func (n *noopDataReader) GetChart(ctx context.Context, userID, chartID string) (*model.Chart, error) {
	return nil, nil
}
func (n *noopDataReader) GetItem(ctx context.Context, userID, itemID string) (*model.Item, error) {
	return nil, nil
}
func (n *noopDataReader) ListItemFolders(ctx context.Context, userID string) ([]*model.Item, error) {
	return []*model.Item{}, nil
}
func (n *noopDataReader) ListAllItems(ctx context.Context, userID string) ([]*model.Item, error) {
	return []*model.Item{}, nil
}

func getHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return fmt.Sprintf("unknown-%d", time.Now().UnixNano()%10000)
	}
	return h
}
