package fileproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Proxy 檔案代理服務
type Proxy struct {
	config     *Config
	cache      *Cache
	httpClient *http.Client
	fetchLocks sync.Map
	bufferPool sync.Pool
}

// fetchLock 用於協調同一檔案的並發下載
type fetchLock struct {
	mu   sync.Mutex
	cond *sync.Cond
	done bool
	err  error
}

func newFetchLock() *fetchLock {
	fl := &fetchLock{}
	fl.cond = sync.NewCond(&fl.mu)
	return fl
}

// NewProxy 建立代理實例
func NewProxy(cfg *Config) (*Proxy, error) {
	cache, err := NewCache(cfg)
	if err != nil {
		return nil, err
	}

	return &Proxy{
		config: cfg,
		cache:  cache,
		httpClient: &http.Client{
			Timeout: cfg.UpstreamTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        cfg.MaxIdleConns,
				MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		bufferPool: sync.Pool{
			New: func() any {
				buf := make([]byte, 32*1024)
				return &buf
			},
		},
	}, nil
}

// Close 關閉代理
func (p *Proxy) Close() error {
	p.cache.Close()
	return nil
}

func (p *Proxy) getBuffer() []byte    { return *p.bufferPool.Get().(*[]byte) }
func (p *Proxy) putBuffer(buf []byte) { p.bufferPool.Put(&buf) }

// ServeHTTP 處理 HTTP 請求
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Path
	if err := p.handleRequest(w, r, key); err != nil {
		slog.Error("request failed", "key", key, "error", err)
	}
}

// handleRequest 處理具體請求
func (p *Proxy) handleRequest(w http.ResponseWriter, r *http.Request, key string) error {
	// 檢查 404 快取
	if p.cache.IsNotFound(key) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return nil
	}

	// 檢查檔案快取
	if entry, ok := p.cache.Get(key); ok {
		if p.validateCacheFile(entry) {
			return p.serveFromCache(w, r, entry)
		}
		slog.Debug("cache file invalid, re-fetching", "key", key)
		p.cache.Remove(key)
	}

	return p.fetchAndServe(r.Context(), w, r, key)
}

// validateCacheFile 驗證快取檔案
func (p *Proxy) validateCacheFile(entry *CacheEntry) bool {
	info, err := os.Stat(entry.FilePath)
	return err == nil && info.Size() == entry.Size
}

// serveFromCache 從快取提供檔案（支援 Range）
func (p *Proxy) serveFromCache(w http.ResponseWriter, r *http.Request, entry *CacheEntry) error {
	file, err := os.Open(entry.FilePath)
	if err != nil {
		return fmt.Errorf("open cache file: %w", err)
	}
	defer file.Close()

	w.Header().Set("Content-Type", entry.ContentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Cache", "HIT")

	// 處理 Range 請求
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		return p.serveRangeFromFile(w, file, entry.Size, rangeHeader)
	}

	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))

	if r.Method == http.MethodHead {
		return nil
	}

	buf := p.getBuffer()
	defer p.putBuffer(buf)
	_, err = io.CopyBuffer(w, file, buf)
	return err
}

// serveRangeFromFile 處理 Range 請求
func (p *Proxy) serveRangeFromFile(w http.ResponseWriter, file *os.File, totalSize int64, rangeHeader string) error {
	start, end, ok := parseRange(rangeHeader, totalSize)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", totalSize))
		http.Error(w, "Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return nil
	}

	length := end - start + 1
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusPartialContent)

	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return err
	}

	buf := p.getBuffer()
	defer p.putBuffer(buf)
	_, err := io.CopyBuffer(w, io.LimitReader(file, length), buf)
	return err
}

// parseRange 解析 Range header
func parseRange(rangeHeader string, totalSize int64) (start, end int64, ok bool) {
	// 僅支援 bytes=start-end 或 bytes=start- 格式
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, false
	}

	spec := strings.TrimPrefix(rangeHeader, "bytes=")
	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		return 0, 0, false
	}

	var err error
	if parts[0] == "" {
		// bytes=-N (最後 N 個位元組)
		end = totalSize - 1
		start, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		start = totalSize - start
		if start < 0 {
			start = 0
		}
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		if parts[1] == "" {
			end = totalSize - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return 0, 0, false
			}
		}
	}

	if start > end || start >= totalSize {
		return 0, 0, false
	}
	if end >= totalSize {
		end = totalSize - 1
	}

	return start, end, true
}

// fetchAndServe 從上游獲取並提供檔案
func (p *Proxy) fetchAndServe(ctx context.Context, w http.ResponseWriter, r *http.Request, key string) error {
	lockI, _ := p.fetchLocks.LoadOrStore(key, newFetchLock())
	lock := lockI.(*fetchLock)

	lock.mu.Lock()

	// 如果下載已完成，從快取讀取
	if lock.done {
		err := lock.err
		lock.mu.Unlock()

		if err != nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return nil
		}
		return p.serveFromCacheOrError(w, r, key)
	}

	// 檢查是否有其他請求正在下載（pending 存在）
	if sf, exists := p.cache.GetPending(key); exists {
		lock.mu.Unlock()
		return p.serveFromStreaming(w, r, sf)
	}

	lock.mu.Unlock()
	return p.doFetchAndServe(ctx, w, r, key, lock)
}

// serveFromCacheOrError 從快取服務或返回錯誤
func (p *Proxy) serveFromCacheOrError(w http.ResponseWriter, r *http.Request, key string) error {
	if p.cache.IsNotFound(key) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return nil
	}
	if entry, ok := p.cache.Get(key); ok && p.validateCacheFile(entry) {
		return p.serveFromCache(w, r, entry)
	}
	p.cache.Remove(key)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	return fmt.Errorf("cache entry invalid after download")
}

// doFetchAndServe 執行實際的下載和回應
func (p *Proxy) doFetchAndServe(ctx context.Context, w http.ResponseWriter, r *http.Request, key string, lock *fetchLock) error {
	defer p.fetchLocks.Delete(key)

	upstreamURL := strings.TrimSuffix(p.config.UpstreamURL, "/") + key

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		p.finishLock(lock, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		p.finishLock(lock, err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		p.finishLock(lock, fmt.Errorf("not found"))
		p.cache.PutNotFound(key)
		http.Error(w, "Not Found", http.StatusNotFound)
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		p.finishLock(lock, fmt.Errorf("upstream: %d", resp.StatusCode))
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return fmt.Errorf("upstream error: %d", resp.StatusCode)
	}

	expectedSize := resp.ContentLength
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	sf, isNew, err := p.cache.GetOrCreatePending(key)
	if err != nil {
		p.finishLock(lock, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return fmt.Errorf("create cache file: %w", err)
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	if expectedSize >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(expectedSize, 10))
	}
	w.Header().Set("X-Cache", "MISS")

	if r.Method == http.MethodHead {
		if isNew {
			p.cache.FailPending(key)
		}
		p.finishLock(lock, nil)
		return nil
	}

	buf := p.getBuffer()
	defer p.putBuffer(buf)

	var totalWritten int64
	var downloadErr error

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if isNew {
				if _, writeErr := sf.Write(buf[:n]); writeErr != nil {
					slog.Warn("cache write failed", "key", key, "error", writeErr)
					isNew = false // 停止寫入快取
				}
			}

			written, writeErr := w.Write(buf[:n])
			totalWritten += int64(written)
			if writeErr != nil {
				downloadErr = fmt.Errorf("write response: %w", writeErr)
				break
			}

			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}

		if readErr != nil {
			if readErr != io.EOF {
				downloadErr = fmt.Errorf("read upstream: %w", readErr)
			}
			break
		}
	}

	if downloadErr != nil {
		if isNew {
			p.cache.FailPending(key)
		}
		p.finishLock(lock, downloadErr)
		return downloadErr
	}

	if expectedSize >= 0 && totalWritten != expectedSize {
		slog.Warn("size mismatch", "key", key, "expected", expectedSize, "got", totalWritten)
		if isNew {
			p.cache.FailPending(key)
		}
		p.finishLock(lock, fmt.Errorf("size mismatch"))
		return fmt.Errorf("size mismatch: expected %d, got %d", expectedSize, totalWritten)
	}

	if isNew {
		p.cache.CompletePending(key, totalWritten, contentType)
	}

	p.finishLock(lock, nil)
	return nil
}

// finishLock 完成鎖定
func (p *Proxy) finishLock(lock *fetchLock, err error) {
	lock.mu.Lock()
	lock.done = true
	lock.err = err
	lock.cond.Broadcast()
	lock.mu.Unlock()
}

// serveFromStreaming 從正在下載的串流讀取
func (p *Proxy) serveFromStreaming(w http.ResponseWriter, r *http.Request, sf *StreamingFile) error {
	w.Header().Set("X-Cache", "STREAMING")

	if r.Method == http.MethodHead {
		return nil
	}

	reader := sf.NewReader()
	defer reader.Close()

	buf := p.getBuffer()
	defer p.putBuffer(buf)

	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write response: %w", writeErr)
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return fmt.Errorf("read streaming file: %w", readErr)
		}
	}

	return nil
}

// Stats 返回代理統計資訊
func (p *Proxy) Stats() map[string]any {
	return p.cache.Stats()
}
