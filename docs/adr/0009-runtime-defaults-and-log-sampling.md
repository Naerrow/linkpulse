# ADR 0009 — 런타임 기본값은 차트 ConfigMap에, 요청 로그는 표본으로

- 상태: 채택 (2026-10-15, plan 0016). 측정 창 확인 전이다 — 결과는 `load/k6/results/0016-followups.md`.
- 관련: AGENTS.md §5(자유 이동 — 같은 차트, 사이트별 값만 다르게), [plan 0016](../plans/0016-observability-followups/plan.md),
  [ADR 0008](0008-in-cluster-prometheus.md)(지표·Fluent Bit), [ADR 0007](0007-ecs-to-eks.md)(파드 250m).

## 맥락

- 0015 실측: 파드 CPU 한도가 250m인데 Go 런타임이 `GOMAXPROCS`를 **2**로 잡았다. 1로 두면 파드 CPU가 10~15% 줄고 지연은 나빠지지 않았다(800 rps까지, E4).
  Go 문서(`go doc runtime.GOMAXPROCS`)는 cgroup 한도로 기본값을 정하되 "2 미만으로 두지 않는다"고 한다.
- 0015 실측: 3200 rps에서 Fluent Bit가 노드 3대 합 719m를 썼다(앱 파드 합 약 1,166m). 요청마다 쓰는 JSON 한 줄이 원인이다.
- 두 값 모두 사이트와 무관하다(P6 k3s도 같은 파드 크기). 그런데 장애·측정 때는 사이트가 바꿀 수 있어야 한다.

## 결정

1. **런타임 기본값은 차트 ConfigMap `<fullname>-defaults`에 둔다**(`values.yaml` `runtimeDefaults`). `envFrom` 순서는 차트 기본값 → 사이트 ConfigMap → 사이트 Secret이다.
   - 쿠버네티스는 같은 키를 뒤의 출처 값으로 쓰고, `env`는 `envFrom`을 늘 이긴다(API `Container.envFrom` 설명). 그래서 기본값을 `env`에 두면 사이트가 바꿀 수 없다.
   - 차트 ConfigMap은 ArgoCD가 소유한다(selfHeal). 바꿀 때는 사이트 ConfigMap에 같은 키를 넣는다.
   - 파드 템플릿에 `checksum/defaults`를 단다. 앱은 기동 때만 환경 변수를 읽으므로 기본값이 바뀌면 파드를 다시 띄운다.
   - 지키는 것: 렌더 단언(이름·순서·`env`에 키 없음·체크섬이 값을 따라 바뀜)과 변이 검사(`scripts/check-charts.sh`).
2. **`GOMAXPROCS=1`**(CPU 한도 250m, 1코어 미만). 한도를 1코어 이상으로 바꾸면 다시 정한다. 명시하면 런타임의 자동 갱신도 꺼진다.
3. **요청 로그 표본화**(`ACCESS_LOG_SAMPLE`=N, 차트 기본 100, 앱 기본 1 = 로컬·docker-compose는 예전과 같다).
   - **늘 남긴다**: 4xx·5xx(런북의 5xx 쿼리·401 grep), 100ms 이상(SLO 지연 경계 — 런북의 1초 초과 쿼리 포함).
   - 나머지(2xx·3xx이고 100ms 미만)는 1/N 확률로 남긴다. 무작위인 이유: 계수기는 요청 순서의 주기(k6가 링크를 차례로 돈다)와 맞물리면 특정 링크만 남긴다. 공유 상태도 없다.
   - **요청 수는 로그로 세지 않는다.** 지표(ALB `RequestCount`·Prometheus `linkpulse_http_requests_total`)는 표본과 무관하게 모든 요청을 센다 — 지표를 먼저 기록한 뒤 로그 여부를 정한다.
   - 0·음수·숫자가 아니면 기동을 막는다. 0이 "로그 끔"으로 읽혀 오류 줄까지 없는 줄 알고 헤매지 않게 한다.
   - 장애 때 전부 남기기: 사이트 ConfigMap `ACCESS_LOG_SAMPLE=1` + 재기동(런북 R-7).

## 트레이드오프 (받아들인 위험)

- **성공 요청의 개별 줄이 99% 사라진다.** "이 링크가 몇 번 열렸나"를 로그로 답할 수 없다 — 클릭 수는 DB, 요청 수는 지표로 본다. 특정 요청을 추적해야 하면 R-7로 잠시 전부 남긴다.
- **`GOMAXPROCS=1`은 GC나 긴 요청이 하나뿐인 P를 막아 꼬리 지연을 늘릴 수 있다.** 0015는 800 rps까지만 쟀다. 0016 창에서 1600·3200 단계의 정지 밖 p99를 비교하고, 기준(r0 대비 2ms와 20% 중 큰 값)을 넘으면 기본값을 `"2"`로 되돌리는 PR을 낸다.
- **기본값이 차트와 사이트 두 곳에 있을 수 있다.** 실효값은 파드 지표(`go_sched_gomaxprocs_threads`)와 기동 로그(`access_log_sample`)로 확인한다.

## 결과

- 차트 하나로 AWS·집이 같은 기본값을 쓰고, 사이트는 키 하나로 덮어쓴다.
- 판정은 0016 측정 창: (a) `GOMAXPROCS` CPU·지연, (d) 3200 rps에서 분당 요청 줄 수 ≈ 늘 남는 요청 + 나머지/100, Fluent Bit CPU·로그 바이트 감소.
