package main

import (
	"context"
	"encoding/json"
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

	// Ensure chat_messages table exists
	if err := pgStore.EnsureChatMessagesTable(ctx); err != nil {
		log.Fatalf("EnsureChatMessagesTable 失敗: %v", err)
	}
	if err := pgStore.EnsureSessionMappingConstraint(ctx); err != nil {
		log.Printf("EnsureSessionMappingConstraint 警告: %v", err)
	}
	if err := pgStore.EnsureVaultSnapshotsTable(ctx); err != nil {
		log.Fatalf("EnsureVaultSnapshotsTable 失敗: %v", err)
	}

	// VaultFS（EFS 實作 + DB 快照自動維護）
	realFS := &mirror.RealVaultFS{Root: cfg.VaultRoot}
	vaultFS := mirror.NewSnapshotAwareVaultFS(realFS, pgStore)

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
		fullExporter: pgStore,
		ctx:          ctx,
	}
	taskStore := selectTaskStore(api.NewMemoryTaskStore(), rdb, cfg.TaskTimeoutMinutes)

	projector := vaultsync.NewSyncEventHandler(vaultFS, pgStore)
	projector.SetLocker(vaultLock)
	projector.StartCacheEvictor(ctx)
	worker := vaultsync.NewChangeWorker(
		pgStore,
		vaultsync.NewVaultChangeProcessor(projector),
		vaultsync.NewFullExportBootstrapper(vaultFS, pgStore),
		vaultLock,
		time.Duration(cfg.SyncLoopIntervalSec)*time.Second,
		"mirror-"+getHostname(),
	)
	worker.SetLeaseTTL(time.Duration(cfg.SyncCursorLeaseSec) * time.Second)
	worker.SetOwnerScanLimit(cfg.SyncOwnerScanLimit)
	worker.SetChangeBatchSize(cfg.SyncChangeBatchSize)

	// API server
	taskHandler := api.NewTaskHandler(taskExec, taskStore)
	taskHandler.SetContext(ctx)
	mux := http.NewServeMux()
	taskHandler.RegisterRoutes(mux)

	chatHandler := api.NewChatHandler(pgStore, vaultFS)
	chatHandler.RegisterRoutes(mux)

	wsHandler := api.NewWsHandler(taskExec, taskStore, pgStore, pgStore, cfg.VaultRoot, vaultFS)
	wsHandler.RegisterRoutes(mux)
	chatHandler.SetWsHandler(wsHandler)

	schemaHandler := api.NewSchemaHandler(vaultFS)
	schemaHandler.RegisterRoutes(mux)

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"status": "ok",
			"worker": worker.Snapshot(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})

	server := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: mux,
	}

	g, gCtx := errgroup.WithContext(ctx)

	// PG-only change worker
	g.Go(func() error {
		worker.Start(gCtx)
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
	fullExporter vaultsync.FullExporter
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

	// 確保 Vault 已初始化（.vault_initialized 不存在時做全量匯出）
	if e.fullExporter != nil && !e.vaultFS.Exists(task.UserID+"/.vault_initialized") {
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
	output, err := e.claudeExec.ExecuteTask(execCtx, task.ID, workDir, task.Instruction, task.Scope, task.UserID)
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
	}

	return nil
}

// ExecuteStream runs the full vault pipeline with streaming Claude CLI output.
// CLI execution is NOT locked — multiple conversations can run concurrently.
// Only the writeback phase acquires the vault lock to prevent DB conflicts.
func (e *fullTaskExecutor) ExecuteStream(task *api.Task, eventCh chan<- executor.StreamEvent) (bool, error) {
	workDir := fmt.Sprintf("%s/%s", e.vaultRoot, task.UserID)

	e.vaultFS.MkdirAll(task.UserID)

	execCtx := e.ctx
	if execCtx == nil {
		execCtx = context.Background()
	}

	// Ensure Vault is initialised (best-effort, no lock needed for read check)
	if e.fullExporter != nil && !e.vaultFS.Exists(task.UserID+"/.vault_initialized") {
		// Lock briefly for full export only
		if e.vaultLock.Lock(task.UserID, task.ID+"-init") {
			if err := vaultsync.ExportFullVault(execCtx, e.vaultFS, e.fullExporter, task.UserID); err != nil {
				log.Printf("[Task %s] full export error: %v", task.ID, err)
			}
			e.vaultLock.Unlock(task.UserID, task.ID+"-init")
		}
	}

	aiStartUSN := 0
	if e.latestUSN != nil {
		if usn, usnErr := e.latestUSN.GetLatestUSN(execCtx, task.UserID); usnErr == nil {
			aiStartUSN = usn
		}
	}

	beforeSnap, beforeIDMap, snapErr := executor.TakeSnapshotAndPathIDMap(e.vaultFS, task.UserID)
	if snapErr != nil {
		return false, fmt.Errorf("snapshot before error: %w", snapErr)
	}

	// Claude CLI streaming — NO LOCK, multiple conversations can run concurrently
	err := e.claudeExec.ExecuteTaskStream(
		execCtx, task.ID, workDir, task.Instruction, task.Scope, task.UserID, eventCh,
	)
	if err != nil {
		return false, err
	}

	// diff + writeback — LOCK only for the writeback phase
	afterSnap, snapErr := executor.TakeSnapshot(e.vaultFS, task.UserID)
	if snapErr != nil {
		return false, fmt.Errorf("snapshot after error: %w", snapErr)
	}
	diff := executor.ComputeDiff(beforeSnap, afterSnap)
	hasChanges := len(diff.Created)+len(diff.Modified)+len(diff.Deleted)+len(diff.Moved) > 0
	if !hasChanges {
		return false, nil
	}

	importer := mirror.NewImporter(e.vaultFS)
	movedEntries := make([]mirror.MovedFileEntry, len(diff.Moved))
	for i, m := range diff.Moved {
		movedEntries[i] = mirror.MovedFileEntry{OldPath: m.OldPath, NewPath: m.NewPath}
	}
	entries, parseErr := importer.ProcessDiff(task.UserID, diff.Created, diff.Modified, diff.Deleted, movedEntries, beforeIDMap)
	if parseErr != nil {
		return false, fmt.Errorf("parse diff error: %w", parseErr)
	}

	// Acquire lock only for DB writeback
	if !e.vaultLock.Lock(task.UserID, task.ID+"-wb") {
		log.Printf("[Task %s] writeback lock contention, proceeding anyway", task.ID)
	} else {
		defer e.vaultLock.Unlock(task.UserID, task.ID+"-wb")
	}

	if e.writer != nil && len(entries) > 0 {
		result := executor.WriteBack(execCtx, e.writer, e.usnReader, e.usnInc, task.UserID, entries, aiStartUSN)
		log.Printf("[Task %s] writeback: +%d ~%d mv%d -%d skip%d err%d",
			task.ID, result.Created, result.Updated, result.Moved, result.Deleted, result.Skipped, result.Errors)

		if e.usnSyncer != nil {
			_ = e.usnSyncer.SyncUserUSN(execCtx, task.UserID)
		}
	}

	return true, nil
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

func (n *noopDataReader) GetItem(ctx context.Context, userID, itemID string) (*model.Item, error) {
	return nil, nil
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
