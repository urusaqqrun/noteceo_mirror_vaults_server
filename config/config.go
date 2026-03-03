package config

import "os"

type Config struct {
	MongoURI     string
	MongoDB      string
	RedisURI     string
	VaultRoot    string
	AnthropicKey string
	Port         string

	// 並發控制
	MaxConcurrentTasks int
	TaskTimeoutMinutes int

	// USN 輪詢
	USNPollIntervalSec int
}

func Load() *Config {
	return &Config{
		MongoURI:           getEnv("MONGO_URI", "mongodb://localhost:27017"),
		MongoDB:            getEnv("MONGO_DB", "NoteCEO"),
		RedisURI:           getEnv("REDIS_URI", "localhost:6379"),
		VaultRoot:          getEnv("VAULT_ROOT", "/vaults"),
		AnthropicKey:       getEnv("ANTHROPIC_API_KEY", ""),
		Port:               getEnv("PORT", "8080"),
		MaxConcurrentTasks: getEnvInt("MAX_CONCURRENT_TASKS", 3),
		TaskTimeoutMinutes: getEnvInt("TASK_TIMEOUT_MINUTES", 10),
		USNPollIntervalSec: getEnvInt("USN_POLL_INTERVAL_SEC", 30),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	for _, c := range v {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	return n
}
