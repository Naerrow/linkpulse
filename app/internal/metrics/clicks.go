package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Naerrow/linkpulse/app/internal/clicks"
)

// clickCounters는 clicks.Stats 필드와 지표 이름의 짝이다. 긁을 때마다 원자 카운터를 그대로 읽는다 —
// 정합성 판정(plan 0015 (b))이 이 값을 정수로 비교하므로 증가분을 따로 모으지 않는다.
// "click queue" 로그 줄(증가분)은 그대로 남는다. 0012~0014 비교와 런북 쿼리가 쓴다.
var clickCounters = []struct {
	name, help string
	value      func(*clicks.Stats) int64
}{
	{"linkpulse_clicks_recorded_total", "리다이렉트가 기록한 클릭 수(Record 호출).", func(s *clicks.Stats) int64 { return s.Recorded.Load() }},
	{"linkpulse_clicks_dropped_total", "대기 상한에 걸려 버린 클릭 수(유실).", func(s *clicks.Stats) int64 { return s.Dropped.Load() }},
	{"linkpulse_clicks_published_batches_total", "큐에 보낸 배치 수.", func(s *clicks.Stats) int64 { return s.PublishedBatches.Load() }},
	{"linkpulse_clicks_published_total", "큐에 보낸 클릭 수.", func(s *clicks.Stats) int64 { return s.PublishedClicks.Load() }},
	{"linkpulse_clicks_publish_errors_total", "배치 발행 실패 수(재시도 목록에 남는다).", func(s *clicks.Stats) int64 { return s.PublishErrors.Load() }},
	{"linkpulse_clicks_applied_batches_total", "DB에 반영한 배치 수.", func(s *clicks.Stats) int64 { return s.AppliedBatches.Load() }},
	{"linkpulse_clicks_applied_total", "DB에 실제로 더한 클릭 수(없는 코드 제외).", func(s *clicks.Stats) int64 { return s.AppliedClicks.Load() }},
	{"linkpulse_clicks_duplicate_batches_total", "이미 반영한 배치가 다시 온 수.", func(s *clicks.Stats) int64 { return s.DuplicateBatches.Load() }},
	{"linkpulse_clicks_unknown_total", "없는 코드라 건너뛴 클릭 수.", func(s *clicks.Stats) int64 { return s.UnknownClicks.Load() }},
	{"linkpulse_clicks_apply_errors_total", "반영 실패와 메시지 해석 실패 수.", func(s *clicks.Stats) int64 { return s.ApplyErrors.Load() }},
}

// pendingCodesDesc는 아직 큐에 못 보낸 코드 수(버퍼 + 재시도 목록) 게이지다.
var pendingCodesDesc = prometheus.NewDesc("linkpulse_clicks_pending_codes", "아직 큐에 보내지 못한 코드 수(버퍼 + 재시도 목록).", nil, nil)

// clickCollector는 clicks.Stats를 긁을 때 읽어 내는 수집기다. 파드마다 프로세스가 시작될 때 0에서 센다.
type clickCollector struct {
	stats   *clicks.Stats
	pending func() int
}

var clickDescs = func() []*prometheus.Desc {
	descs := make([]*prometheus.Desc, len(clickCounters))
	for i, c := range clickCounters {
		descs[i] = prometheus.NewDesc(c.name, c.help, nil, nil)
	}
	return descs
}()

func (c *clickCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range clickDescs {
		ch <- d
	}
	ch <- pendingCodesDesc
}

func (c *clickCollector) Collect(ch chan<- prometheus.Metric) {
	for i, cc := range clickCounters {
		ch <- prometheus.MustNewConstMetric(clickDescs[i], prometheus.CounterValue, float64(cc.value(c.stats)))
	}
	ch <- prometheus.MustNewConstMetric(pendingCodesDesc, prometheus.GaugeValue, float64(c.pending()))
}
