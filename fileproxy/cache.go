package fileproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

const indexFileName = "index.json"

// CacheEntry 快取條目
type CacheEntry struct {
	Key         string    `json:"key"`
	FilePath    string    `json:"file_path"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
}

// cacheIndex 快取索引（用於持久化）
type cacheIndex struct {
	Entries []*CacheEntry `json:"entries"`
}

// Cache 檔案快取系統
type Cache struct {
	config        *Config
	fileCache     *expirable.LRU[string, *CacheEntry]
	notFoundCache *expirable.LRU[string, struct{}]
	totalSize     atomic.Int64

	pending   map[string]*StreamingFile
	pendingMu sync.RWMutex

	closeCh chan struct{}
	wg      sync.WaitGroup
}

// NewCache 建立快取實例
func NewCache(cfg *Config) (*Cache, error) {
	if err := os.MkdirAll(cfg.CacheDir, 0755); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}

	c := &Cache{
		config:  cfg,
		pending: make(map[string]*StreamingFile),
		closeCh: make(chan struct{}),
	}

	c.fileCache = expirable.NewLRU[string, *CacheEntry](
		0,
		func(key string, entry *CacheEntry) {
			if entry != nil && entry.FilePath != "" {
				os.Remove(entry.FilePath)
			}
			if entry != nil {
				c.totalSize.Add(-entry.Size)
				slog.Debug("cache evicted", "key", key, "size", entry.Size)
			}
		},
		cfg.DefaultCacheTTL,
	)

	c.notFoundCache = expirable.NewLRU[string, struct{}](
		10000,
		nil,
		cfg.NotFoundCacheTTL,
	)

	if err := c.loadAndCleanup(); err != nil {
		slog.Warn("load cache index failed", "error", err)
	}

	c.wg.Add(1)
	go c.saveLoop()

	return c, nil
}

// Close 關閉快取系統
func (c *Cache) Close() {
	close(c.closeCh)
	c.wg.Wait()
	if err := c.saveIndex(); err != nil {
		slog.Warn("save cache index failed", "error", err)
	}
}

// loadAndCleanup 載入快取索引並清理孤立檔案
func (c *Cache) loadAndCleanup() error {
	// 載入索引
	indexPath := filepath.Join(c.config.CacheDir, indexFileName)
	validFiles := make(map[string]bool)

	data, err := os.ReadFile(indexPath)
	if err == nil {
		var idx cacheIndex
		if err := json.Unmarshal(data, &idx); err == nil {
			loaded := 0
			for _, entry := range idx.Entries {
				info, err := os.Stat(entry.FilePath)
				if err != nil || info.Size() != entry.Size {
					os.Remove(entry.FilePath)
					continue
				}
				c.fileCache.Add(entry.Key, entry)
				c.totalSize.Add(entry.Size)
				validFiles[entry.FilePath] = true
				loaded++
			}
			slog.Info("cache index loaded", "entries", loaded)
		}
	}

	// 掃描並清理孤立檔案
	return c.cleanupOrphanFiles(validFiles)
}

// cleanupOrphanFiles 清理不在快取清單中的檔案
func (c *Cache) cleanupOrphanFiles(validFiles map[string]bool) error {
	indexPath := filepath.Join(c.config.CacheDir, indexFileName)
	tmpIndexPath := indexPath + ".tmp"
	removed := 0

	err := filepath.WalkDir(c.config.CacheDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 忽略錯誤繼續掃描
		}
		if d.IsDir() {
			return nil
		}
		// 跳過索引檔案
		if path == indexPath || path == tmpIndexPath {
			return nil
		}
		// 檢查是否為有效快取檔案
		if !validFiles[path] {
			os.Remove(path)
			removed++
		}
		return nil
	})

	if removed > 0 {
		slog.Info("orphan files cleaned", "count", removed)
	}

	// 清理空目錄
	c.cleanupEmptyDirs()

	return err
}

// cleanupEmptyDirs 清理空目錄
func (c *Cache) cleanupEmptyDirs() {
	entries, err := os.ReadDir(c.config.CacheDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		subdir := filepath.Join(c.config.CacheDir, entry.Name())
		subEntries, err := os.ReadDir(subdir)
		if err == nil && len(subEntries) == 0 {
			os.Remove(subdir)
		}
	}
}

// saveIndex 保存快取索引
func (c *Cache) saveIndex() error {
	keys := c.fileCache.Keys()
	idx := cacheIndex{Entries: make([]*CacheEntry, 0, len(keys))}

	for _, key := range keys {
		if entry, ok := c.fileCache.Peek(key); ok {
			idx.Entries = append(idx.Entries, entry)
		}
	}

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}

	indexPath := filepath.Join(c.config.CacheDir, indexFileName)
	tmpPath := indexPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write index: %w", err)
	}
	if err := os.Rename(tmpPath, indexPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename index: %w", err)
	}

	slog.Debug("cache index saved", "entries", len(idx.Entries))
	return nil
}

// saveLoop 定期保存索引
func (c *Cache) saveLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-c.closeCh:
			return
		case <-ticker.C:
			if err := c.saveIndex(); err != nil {
				slog.Warn("save cache index failed", "error", err)
			}
		}
	}
}

// filePath 產生檔案路徑
func (c *Cache) filePath(key string) string {
	hash := sha256.Sum256([]byte(key))
	hashStr := hex.EncodeToString(hash[:])
	return filepath.Join(c.config.CacheDir, hashStr[:2], hashStr)
}

// Get 取得快取條目
func (c *Cache) Get(key string) (*CacheEntry, bool) {
	if _, ok := c.notFoundCache.Get(key); ok {
		c.notFoundCache.Add(key, struct{}{}) // 刷新 TTL
		return nil, false                    // 返回 false 表示是 404 快取
	}
	if entry, ok := c.fileCache.Get(key); ok {
		c.fileCache.Add(key, entry) // 刷新 TTL
		return entry, true
	}
	return nil, false
}

// IsNotFound 檢查是否為 404 快取
func (c *Cache) IsNotFound(key string) bool {
	if _, ok := c.notFoundCache.Get(key); ok {
		c.notFoundCache.Add(key, struct{}{})
		return true
	}
	return false
}

// GetOrCreatePending 取得或建立待下載的串流檔案
func (c *Cache) GetOrCreatePending(key string) (*StreamingFile, bool, error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	if sf, ok := c.pending[key]; ok {
		return sf, false, nil
	}

	filePath := c.filePath(key)
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return nil, false, fmt.Errorf("create cache subdirectory: %w", err)
	}

	sf, err := NewStreamingFile(filePath)
	if err != nil {
		return nil, false, err
	}

	c.pending[key] = sf
	return sf, true, nil
}

// GetPending 取得正在下載的串流檔案
func (c *Cache) GetPending(key string) (*StreamingFile, bool) {
	c.pendingMu.RLock()
	defer c.pendingMu.RUnlock()
	sf, ok := c.pending[key]
	return sf, ok
}

// CompletePending 完成下載
func (c *Cache) CompletePending(key string, size int64, contentType string) {
	c.pendingMu.Lock()
	sf, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.pendingMu.Unlock()

	if !ok {
		return
	}

	sf.Complete()
	c.evictIfNeeded(size)

	entry := &CacheEntry{
		Key:         key,
		FilePath:    c.filePath(key),
		Size:        size,
		ContentType: contentType,
		CreatedAt:   time.Now(),
	}

	c.fileCache.Add(key, entry)
	c.totalSize.Add(size)
}

// evictIfNeeded 如果超出大小限制，淘汰最舊的條目
func (c *Cache) evictIfNeeded(incoming int64) {
	for c.totalSize.Load()+incoming > c.config.MaxCacheSize {
		if _, _, ok := c.fileCache.RemoveOldest(); !ok {
			break
		}
	}
}

// FailPending 下載失敗
func (c *Cache) FailPending(key string) {
	c.pendingMu.Lock()
	sf, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.pendingMu.Unlock()

	if ok {
		sf.Abort()
	}
}

// PutNotFound 快取未找到的結果
func (c *Cache) PutNotFound(key string) {
	c.notFoundCache.Add(key, struct{}{})
}

// Remove 移除快取條目
func (c *Cache) Remove(key string) {
	c.fileCache.Remove(key)
	c.notFoundCache.Remove(key)
}

// Stats 返回快取統計資訊
func (c *Cache) Stats() map[string]any {
	c.pendingMu.RLock()
	pending := len(c.pending)
	c.pendingMu.RUnlock()

	return map[string]any{
		"file_entries":     c.fileCache.Len(),
		"notfound_entries": c.notFoundCache.Len(),
		"total_size":       c.totalSize.Load(),
		"max_size":         c.config.MaxCacheSize,
		"usage_percent":    float64(c.totalSize.Load()) / float64(c.config.MaxCacheSize) * 100,
		"pending":          pending,
	}
}

// StreamingFile 支援並發讀取的串流檔案
type StreamingFile struct {
	mu       sync.RWMutex
	cond     *sync.Cond
	filePath string
	file     *os.File
	size     int64
	done     bool
	err      error
}

// NewStreamingFile 建立串流檔案
func NewStreamingFile(filePath string) (*StreamingFile, error) {
	file, err := os.Create(filePath)
	if err != nil {
		return nil, fmt.Errorf("create cache file: %w", err)
	}
	sf := &StreamingFile{filePath: filePath, file: file}
	sf.cond = sync.NewCond(&sf.mu)
	return sf, nil
}

// Write 寫入資料
func (sf *StreamingFile) Write(p []byte) (int, error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	if sf.done {
		return 0, fmt.Errorf("streaming file closed")
	}

	n, err := sf.file.Write(p)
	sf.size += int64(n)
	sf.cond.Broadcast()
	return n, err
}

// Complete 完成寫入
func (sf *StreamingFile) Complete() {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.done = true
	sf.file.Close()
	sf.cond.Broadcast()
}

// Abort 中止寫入
func (sf *StreamingFile) Abort() {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.done = true
	sf.err = fmt.Errorf("download aborted")
	sf.file.Close()
	os.Remove(sf.filePath)
	sf.cond.Broadcast()
}

// Size 返回當前大小
func (sf *StreamingFile) Size() int64 {
	sf.mu.RLock()
	defer sf.mu.RUnlock()
	return sf.size
}

// NewReader 建立新的讀取者
func (sf *StreamingFile) NewReader() *StreamingFileReader {
	return &StreamingFileReader{sf: sf}
}

// StreamingFileReader 串流檔案讀取者
type StreamingFileReader struct {
	sf     *StreamingFile
	offset int64
	file   *os.File
}

// Read 讀取資料，若資料尚未準備好會等待
func (r *StreamingFileReader) Read(p []byte) (int, error) {
	if r.file == nil {
		var err error
		r.file, err = os.Open(r.sf.filePath)
		if err != nil {
			return 0, err
		}
	}

	r.sf.mu.Lock()
	for {
		if r.sf.err != nil {
			r.sf.mu.Unlock()
			return 0, r.sf.err
		}

		available := r.sf.size - r.offset
		if available > 0 {
			r.sf.mu.Unlock()

			toRead := int64(len(p))
			if toRead > available {
				toRead = available
			}
			n, err := r.file.ReadAt(p[:toRead], r.offset)
			r.offset += int64(n)
			return n, err
		}

		if r.sf.done {
			r.sf.mu.Unlock()
			return 0, io.EOF
		}

		r.sf.cond.Wait()
	}
}

// Close 關閉讀取者
func (r *StreamingFileReader) Close() error {
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
