-- 网络延迟统计：5 段延迟 + 入口类型 + 客户端地理定位
-- 配套：feature/latency-tracking 分支
-- 参考：plan/latency-tracking-final.md

ALTER TABLE usage_logs
  ADD COLUMN IF NOT EXISTS server_processing_ms  integer,
  ADD COLUMN IF NOT EXISTS upstream_ttfb_ms      integer,
  ADD COLUMN IF NOT EXISTS upstream_stream_ms    integer,
  ADD COLUMN IF NOT EXISTS response_delivery_ms  integer,
  ADD COLUMN IF NOT EXISTS total_latency_ms      integer,
  ADD COLUMN IF NOT EXISTS access_type           varchar(16),
  ADD COLUMN IF NOT EXISTS client_country        varchar(2),
  ADD COLUMN IF NOT EXISTS client_region         varchar(64);

COMMENT ON COLUMN usage_logs.server_processing_ms IS 'Go 内部处理（鉴权/路由/账号选择）耗时';
COMMENT ON COLUMN usage_logs.upstream_ttfb_ms     IS '发出上游请求 → 收到首字节耗时';
COMMENT ON COLUMN usage_logs.upstream_stream_ms   IS '上游流式生成时长';
COMMENT ON COLUMN usage_logs.response_delivery_ms IS 'Go → 客户端交付耗时';
COMMENT ON COLUMN usage_logs.total_latency_ms     IS 'Go 视角端到端总耗时';
COMMENT ON COLUMN usage_logs.access_type          IS '入口类型: domain | direct_ip';
COMMENT ON COLUMN usage_logs.client_country       IS 'ISO 国家代码, 例如 CN/US';
COMMENT ON COLUMN usage_logs.client_region        IS '省/州, 例如 Guangdong/California';
