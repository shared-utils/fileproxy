# File Proxy

HTTP 文件代理服務，支持快取、LRU 淘汰、流式傳輸和 Range 請求。

## 功能特性

- **透明代理**: 路徑直接透傳到上游
- **智能快取**: LRU 淘汰 + 滑動過期 TTL
- **負快取**: 對 404 響應進行快取
- **流式傳輸**: 邊下載邊返回，多請求共享下載流
- **Range 請求**: 支持斷點續傳
- **快取持久化**: 重啟後自動恢復快取狀態

## 安裝

```bash
go build -o fileproxy ./cmd/fileproxy
```

## 使用

```bash
# 基本用法
fileproxy --upstream https://example.com/files

# 完整參數
fileproxy \
  --listen ":8080" \
  --upstream "https://example.com/files" \
  --cache-dir "./cache" \
  --max-cache-gb 2 \
  --cache-ttl 1h \
  --notfound-ttl 5s \
  --debug

# TLS
fileproxy --upstream https://example.com --tls-cert cert.pem --tls-key key.pem

# 環境變量
export UPSTREAM_URL="https://example.com/files"
export DEBUG=true
fileproxy
```

## 參數

| 參數 | 環境變量 | 說明 | 默認值 |
|------|----------|------|--------|
| `--listen` | `LISTEN_ADDR` | 監聽地址 | `:8080` |
| `--upstream` | `UPSTREAM_URL` | 上游服務 URL | - |
| `--cache-dir` | `CACHE_DIR` | 快取目錄 | `./cache` |
| `--max-cache-gb` | `MAX_CACHE_GB` | 最大快取大小 (GB) | `1.0` |
| `--cache-ttl` | `CACHE_TTL` | 快取過期時間 | `1h` |
| `--notfound-ttl` | `NOTFOUND_TTL` | 404 快取時間 | `5s` |
| `--tls-cert` | `TLS_CERT` | TLS 證書文件 | - |
| `--tls-key` | `TLS_KEY` | TLS 私鑰文件 | - |
| `--debug` | `DEBUG` | 啟用調試日誌 | `false` |

## 工作原理

請求 `GET /path/to/file.txt` 會被代理到 `{upstream}/path/to/file.txt`

- 快取命中時延長過期時間（滑動過期）
- 多個請求同一文件時共享下載流
- 支持 `Range` 請求頭（斷點續傳）
- 啟動時自動清理不在索引中的孤立快取文件

## API

| 端點 | 說明 |
|------|------|
| `GET /health` | 健康檢查 |
| `GET /stats` | 快取統計 |
| `GET /*` | 文件代理 |
| `HEAD /*` | 文件頭信息 |

## 響應頭

| 頭 | 說明 |
|----|------|
| `X-Cache: HIT` | 快取命中 |
| `X-Cache: MISS` | 快取未命中，從上游獲取 |
| `X-Cache: STREAMING` | 正在從另一個請求的下載流讀取 |
| `Accept-Ranges: bytes` | 支持 Range 請求 |
