package fileproxy

import (
	"fmt"
	"net/url"
	"time"
)

// Config 代理服務配置
type Config struct {
	ListenAddr       string        // 監聽地址
	UpstreamURL      string        // 上游服務 URL
	CacheDir         string        // 快取目錄
	MaxCacheSize     int64         // 最大快取大小（位元組）
	DefaultCacheTTL  time.Duration // 預設快取過期時間
	NotFoundCacheTTL time.Duration // 未找到快取過期時間

	// HTTP Client 配置
	UpstreamTimeout     time.Duration // 上游請求超時
	MaxIdleConns        int           // 最大空閒連接數
	MaxIdleConnsPerHost int           // 每個 host 最大空閒連接數

	// TLS 配置
	TLSCertFile string // TLS 憑證檔案路徑
	TLSKeyFile  string // TLS 私鑰檔案路徑
}

// DefaultConfig 返回預設配置
func DefaultConfig() *Config {
	return &Config{
		ListenAddr:          ":8080",
		CacheDir:            "./cache",
		MaxCacheSize:        1 << 30, // 1GB
		DefaultCacheTTL:     time.Hour,
		NotFoundCacheTTL:    5 * time.Second,
		UpstreamTimeout:     5 * time.Minute,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
	}
}

// Validate 驗證配置
func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr is required")
	}
	if c.UpstreamURL == "" {
		return fmt.Errorf("upstream_url is required")
	}
	if _, err := url.Parse(c.UpstreamURL); err != nil {
		return fmt.Errorf("invalid upstream_url: %w", err)
	}
	if c.CacheDir == "" {
		return fmt.Errorf("cache_dir is required")
	}
	return nil
}
