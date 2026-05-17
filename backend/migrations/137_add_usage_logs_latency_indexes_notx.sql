-- 延迟分析查询所需的并发索引
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_created_total_latency
  ON usage_logs (created_at, total_latency_ms);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_country_created
  ON usage_logs (client_country, created_at);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_access_type_created
  ON usage_logs (access_type, created_at);
