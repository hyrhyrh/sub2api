// Package service - GeoIP 解析（基于 MaxMind GeoLite2-City mmdb）
package service

import (
	"net"
	"os"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/oschwald/geoip2-golang"
	"go.uber.org/zap"
)

// GeoResult 客户端地理定位结果。任一字段为空表示该层级解析失败。
type GeoResult struct {
	Country string // ISO-3166-1 alpha-2，例如 "CN"
	Region  string // 省/州英文名，例如 "Guangdong"
}

// GeoIPService 封装 mmdb 查询。db 为 nil 时所有 Lookup 返回空 GeoResult，
// 不会 panic 也不会阻塞调用方 —— 适合在 mmdb 文件缺失时降级运行。
type GeoIPService struct {
	mu sync.RWMutex
	db *geoip2.Reader
}

// NewGeoIPService 从环境变量 GEOIP_DB_PATH 读取 mmdb 路径并打开。
// 路径为空、文件不存在、或解析失败时返回 db=nil 的降级实例（不返回 error）。
//
// 这样设计是为了让 GeoIP 数据缺失不会阻塞 sub2api 启动；运维补好 mmdb 后
// 重启服务即可恢复解析。
func NewGeoIPService() *GeoIPService {
	svc := &GeoIPService{}
	path := strings.TrimSpace(os.Getenv("GEOIP_DB_PATH"))
	if path == "" {
		logger.LegacyPrintf("service.geoip", "GEOIP_DB_PATH not set, GeoIP lookup disabled")
		return svc
	}
	if _, err := os.Stat(path); err != nil {
		logger.LegacyPrintf("service.geoip", "GeoIP db not accessible at %s: %v (lookup disabled)", path, err)
		return svc
	}
	db, err := geoip2.Open(path)
	if err != nil {
		logger.LegacyPrintf("service.geoip", "Failed to open GeoIP db at %s: %v (lookup disabled)", path, err)
		return svc
	}
	svc.db = db
	logger.L().Info("GeoIP db loaded", zap.String("path", path))
	return svc
}

// Lookup 解析 IP 字符串到国家/省。任何错误（IP 非法、私网、未命中等）均返回空 GeoResult，
// 调用方据此可以无脑写空字符串到 DB。
func (g *GeoIPService) Lookup(ipStr string) GeoResult {
	if g == nil {
		return GeoResult{}
	}
	g.mu.RLock()
	db := g.db
	g.mu.RUnlock()
	if db == nil {
		return GeoResult{}
	}
	ipStr = strings.TrimSpace(ipStr)
	if ipStr == "" {
		return GeoResult{}
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return GeoResult{}
	}
	rec, err := db.City(ip)
	if err != nil || rec == nil {
		return GeoResult{}
	}
	out := GeoResult{Country: rec.Country.IsoCode}
	if len(rec.Subdivisions) > 0 {
		// 优先 en，再 zh-CN，最后任意第一个
		if v, ok := rec.Subdivisions[0].Names["en"]; ok && v != "" {
			out.Region = v
		} else if v, ok := rec.Subdivisions[0].Names["zh-CN"]; ok && v != "" {
			out.Region = v
		} else {
			for _, v := range rec.Subdivisions[0].Names {
				if v != "" {
					out.Region = v
					break
				}
			}
		}
	}
	return out
}

// Close 释放 mmdb 文件句柄。重复调用安全。
func (g *GeoIPService) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.db == nil {
		return nil
	}
	err := g.db.Close()
	g.db = nil
	return err
}
