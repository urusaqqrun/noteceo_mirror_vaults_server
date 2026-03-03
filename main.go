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

	// Mongo reader（若連不上，降級為 no-op）
	var dataReader vaultsync.DataReader = &noopDataReader{}
	mongoReader, err := database.NewMongoReader(ctx, cfg.MongoURI, cfg.MongoDB)
	if err != nil {
		log.Printf("Mongo 連線失敗，事件同步降級為 no-op: %v", err)
	} else {
		defer func() {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			mongoReader.Close(closeCtx)
		}()
		dataReader = mongoReader
	}

	// VaultFS（EFS 實作）
	vaultFS := &mirror.RealVaultFS{Root: cfg.VaultRoot}

	// Claude CLI executor
	claudeExec := executor.NewClaudeExecutor(
		cfg.MaxConcurrentTasks,
		time.Duration(cfg.TaskTimeoutMinutes)*time.Minute,
		cfg.VaultRoot,
	)

	// Vault Lock
	vaultLock := executor.NewVaultLock()

	// Task executor（整合 Claude CLI + VaultFS + Vault Lock）
	taskExec := &fullTaskExecutor{
		claudeExec: claudeExec,
		vaultLock:  vaultLock,
		vaultFS:    vaultFS,
		vaultRoot:  cfg.VaultRoot,
		ctx:        ctx,
	}
	taskStore := selectTaskStore(api.NewMemoryTaskStore(), rdb)

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

	// 背景工作使用 errgroup 管理，確保 graceful shutdown 時等待完成
	g, gCtx := errgroup.WithContext(ctx)

	// Redis Streams consumer
	eventHandler := vaultsync.NewSyncEventHandler(vaultFS, dataReader)
	eventHandler.SetLocker(vaultLock)
	consumer := vaultsync.NewConsumer(rdb, eventHandler, "mirror-1")
	g.Go(func() error {
		if err := consumer.Start(gCtx); err != nil && gCtx.Err() == nil {
			return fmt.Errorf("consumer: %w", err)
		}
		return nil
	})

	// USN poller 兜底（僅在 Mongo 可用時啟動）
	if mongoReader != nil {
		activeUsersFn := func() []string {
			c, cancelFn := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancelFn()
			users, err := mongoReader.ListActiveUsers(c)
			if err != nil {
				log.Printf("list active users failed: %v", err)
				return []string{}
			}
			return users
		}
		poller := vaultsync.NewUSNPoller(
			mongoReader,
			eventHandler,
			time.Duration(cfg.USNPollIntervalSec)*time.Second,
			activeUsersFn,
		)
		g.Go(func() error {
			poller.Start(gCtx)
			return nil
		})
	}

	// HTTP server（背景）
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
	opt.PoolSize = 5
	opt.MinIdleConns = 1

	rdb := redis.NewClient(opt)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Printf("Redis 連線失敗（非致命）: %v", err)
	} else {
		log.Println("Redis 連線成功")
	}
	return rdb
}

// fullTaskExecutor 整合所有執行元件
type fullTaskExecutor struct {
	claudeExec *executor.ClaudeExecutor
	vaultLock  *executor.VaultLock
	vaultFS    mirror.VaultFS
	vaultRoot  string
	ctx        context.Context
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
	output, err := e.claudeExec.ExecuteTask(execCtx, task.ID, workDir, task.Instruction)
	if err != nil {
		return err
	}
	task.Result = output
	return nil
}

func (e *fullTaskExecutor) Cancel(taskID string) error {
	return e.claudeExec.Cancel(taskID)
}

func selectTaskStore(fallback api.TaskStore, rdb *redis.Client) api.TaskStore {
	if rdb != nil {
		if err := rdb.Ping(context.Background()).Err(); err == nil {
			return api.NewRedisTaskStore(rdb)
		}
		log.Printf("Redis 不可用，TaskStore 降級 MemoryStore")
	}
	return fallback
}

// noopDataReader 用於 Mongo 不可用時的降級模式。
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
