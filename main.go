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

	if err := pgStore.EnsureVaultSnapshotsTable(ctx); err != nil {
		log.Fatalf("EnsureVaultSnapshotsTable 失敗: %v", err)
	}

	// VaultFS（EFS 實作 + DB 快照自動維護）
	realFS := &mirror.RealVaultFS{Root: cfg.VaultRoot}
	vaultFS := mirror.NewSnapshotAwareVaultFS(realFS, pgStore)

	// Vault Lock（sync worker 用，優先使用 Redis 分散式鎖）
	var vaultLock executor.VaultLocker = executor.NewVaultLock()
	if rdb != nil {
		vaultLock = executor.NewRedisVaultLock(rdb, 15*time.Minute)
	}

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

	// WorkerClient（僅 Rebuild，用於 VaultSyncHandler 觸發插件編譯）
	var workerClient *api.WorkerClient
	workerURL := os.Getenv("CLI_WORKER_URL")
	workerSecret := os.Getenv("INTERNAL_SECRET")
	if workerURL != "" {
		workerClient = api.NewWorkerClient(workerURL, workerSecret)
		log.Printf("CLI Worker: %s", workerURL)
	} else {
		log.Printf("CLI_WORKER_URL 未設定，plugin rebuild 功能停用")
	}

	// API server
	mux := http.NewServeMux()

	var svcHandler *api.ServiceHandler
	serviceWorkerURL := os.Getenv("SERVICE_WORKER_URL")
	if serviceWorkerURL != "" {
		svcHandler = api.NewServiceHandler(cfg.VaultRoot, serviceWorkerURL, workerSecret)
		svcHandler.RegisterRoutes(mux)
		log.Printf("Service Worker: %s", serviceWorkerURL)
	}

	schemaHandler := api.NewSchemaHandler(vaultFS)
	schemaHandler.RegisterRoutes(mux)

	skillHandler := api.NewSkillHandler(vaultFS)
	skillHandler.RegisterRoutes(mux)

	gitHandler := api.NewPluginGitHandler(cfg.VaultRoot, vaultLock, workerClient)
	gitHandler.RegisterRoutes(mux)

	pluginHandler := api.NewPluginHandler(vaultFS, pgStore, vaultLock, projector, svcHandler, gitHandler)
	pluginHandler.RegisterRoutes(mux)

	vaultSyncHandler := api.NewVaultSyncHandler(vaultFS, cfg.VaultRoot, nil, workerClient)
	vaultSyncHandler.RegisterRoutes(mux)

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

func getHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return fmt.Sprintf("unknown-%d", time.Now().UnixNano()%10000)
	}
	return h
}


