package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/alecthomas/kong"

	"github.com/shared-utils/fileproxy/fileproxy"
)

type CLI struct {
	Listen      string        `help:"Listen address" default:":8080" env:"LISTEN_ADDR"`
	Upstream    string        `help:"Upstream URL" required:"" env:"UPSTREAM_URL"`
	CacheDir    string        `help:"Cache directory" default:"./cache" env:"CACHE_DIR" type:"path"`
	MaxCacheGB  float64       `help:"Max cache size in GB" default:"1.0" name:"max-cache-gb" env:"MAX_CACHE_GB"`
	CacheTTL    time.Duration `help:"Cache TTL" default:"1h" name:"cache-ttl" env:"CACHE_TTL"`
	NotFoundTTL time.Duration `help:"NotFound cache TTL" default:"5s" name:"notfound-ttl" env:"NOTFOUND_TTL"`
	TLSCert     string        `help:"TLS certificate file" name:"tls-cert" env:"TLS_CERT" type:"existingfile"`
	TLSKey      string        `help:"TLS private key file" name:"tls-key" env:"TLS_KEY" type:"existingfile"`
	Debug       bool          `help:"Enable debug logging" env:"DEBUG"`
}

func (c *CLI) Run() error {
	// 初始化 slog
	level := slog.LevelInfo
	if c.Debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg := &fileproxy.Config{
		ListenAddr:          c.Listen,
		UpstreamURL:         c.Upstream,
		CacheDir:            c.CacheDir,
		MaxCacheSize:        int64(c.MaxCacheGB * 1024 * 1024 * 1024),
		DefaultCacheTTL:     c.CacheTTL,
		NotFoundCacheTTL:    c.NotFoundTTL,
		UpstreamTimeout:     5 * time.Minute,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		TLSCertFile:         c.TLSCert,
		TLSKeyFile:          c.TLSKey,
	}

	return fileproxy.Run(cfg)
}

func main() {
	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Name("fileproxy"),
		kong.Description("HTTP file proxy with caching and range support"),
		kong.UsageOnError(),
	)
	if err := ctx.Run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}
