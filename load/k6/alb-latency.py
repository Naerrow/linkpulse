#!/usr/bin/env python3
"""ALB 접근 로그로 대상 처리 시간을 초 단위로 본다(plan 0016 설계 4 — 매 분 :00초 정지).

사용: python3 -I load/k6/alb-latency.py <로그 디렉터리> <시작 epoch> <-prom.json> [MAX_RPS]
  로그 디렉터리: aws s3 sync로 받은 디렉터리(*.log.gz). 파일 이름의 시각으로 실행 구간 근처 파일만 읽는다.
  시작 epoch:   capacity.js setup이 찍은 "시나리오 시작 … epoch=" 값. START_OFFSET_S를 줬으면 정각 + 오프셋이다.
                collect-metrics.sh에는 직전 정각(epoch − 오프셋)을 주지만, 여기에는 실제 값을 준다.
  -prom.json:   collect-metrics.sh가 남긴 그 실행의 Prometheus 원본. alive의 instance로 파드 IP ↔ 이름을 맞춘다.
  MAX_RPS:      k6에 준 값(기본 1600). 단계표를 capacity.js와 같은 규칙으로 자른다.

분 단위 표로는 1~3초짜리 정지가 보이지 않는다. 그래서 요청마다 도착 시각(request_creation_time)과
대상 처리 시간(target_processing_time)을 읽어, 단계의 유지 구간(램프 30초 뒤 2분 30초)을 초 단위로 나눈다.
  - 벽시계 초(0~59)와 k6 시작 기준 초를 따로 센다. 시작이 정각이면 두 기준이 같다.
  - 정지 = 벽시계 :00~:02초 또는 k6 기준 :00~:02초. 정지 밖 = 정지도 벽시계 :30~:32초도 아닌 초.
    :30~:32를 늘 빼는 이유: 30초 비킨 실행(r0)은 그 자리가 k6 기준 정지라 빠진다. 정각 실행도 같은 초를 빼야
    실행 간 정지 밖 p99가 같은 초 집합으로 비교된다(판정 (a)). goroutine-snap.sh의 :30.2초 스냅숏(덤프 중 정지)도 여기 든다.
  - 새 파드 = 실행 중에 생긴 파드의 첫 요청 뒤 2분. 시작 2분 전(setup)부터 요청을 받은 파드는 기존 파드다.
    더 앞을 보지 않는 이유: 직전 실행의 파드 IP를 VPC CNI가 다시 쓰면 새 파드가 기존으로 보인다(실행 사이는 5분 넘게 비어 있다).
  - 재현 판정(plan 0016 판정 (c)): :00초의 50ms 초과 비율이 다른 초 비율의 중앙값보다 5배 이상.
    다른 초 대부분에 느린 요청이 없어 중앙값이 0이면 평균과 비교한다.
클라이언트 IP는 측정 집 IP라 읽지 않는다. 필드 순서는 ELB 사용자 가이드 "Access log entries"를 따른다. 로컬 파일만 읽는다.
"""
import array
import bisect
import collections
import datetime
import gzip
import json
import pathlib
import re
import shlex
import statistics
import sys

RATES = [50, 100, 200, 400, 800, 1600, 3200, 6400]  # capacity.js ALL_RATES와 같아야 한다
STAGE_S, RAMP_S = 180, 30
SLOW_MS = 50  # 느린 요청 경계. 0015 정지 분석(plan 0016 배경)과 같은 값
STALL_S = (0, 1, 2)  # 정지 초
CONTROL_S = (30, 31, 32)  # 정지 밖에서도 빼는 벽시계 초(위 설명 — 실행 간 같은 초 집합)
FIRST_LOOKBACK_S = 120  # 기존 파드를 가리려고 시작 전 요청을 보는 범위(위 설명 — IP 재사용)
NEW_POD_S = 120  # 새 파드로 보는 기간(첫 요청 뒤)
ZOOM_S, ZOOM_BIN = 3, 0.1  # 정지 앞뒤 확대 창과 칸 크기
RATIO = 5  # 재현 판정 배수
FILE_TS = re.compile(r"_(\d{8}T\d{4})Z_")

# 필드 위치(0부터, 공백으로 나눈 앞부분). 요청 줄·User-Agent에 공백이 있어 뒤 필드는 따옴표로 나눠 찾는다.
TARGET, TGT_T = 4, 6


def iso(t):
    return datetime.datetime.fromtimestamp(t, datetime.timezone.utc).strftime("%H:%M:%S")


_day = {}


def epoch(ts):
    """'2026-10-15T01:05:00.541000Z' → epoch 초(소수). 하루 시작 값을 캐시해 줄마다 날짜를 풀지 않는다."""
    base = _day.get(ts[:10])
    if base is None:
        base = _day[ts[:10]] = datetime.datetime.fromisoformat(ts[:10] + "T00:00:00+00:00").timestamp()
    return base + int(ts[11:13]) * 3600 + int(ts[14:16]) * 60 + float(ts[17:-1])


def fields(line):
    """(도착 epoch, 대상 IP, 대상 처리 ms 또는 None) — 대상이 없거나 응답하지 않았으면 IP·시간이 None이다."""
    parts = line.split('"')
    head = parts[0].split()
    if len(head) < 12:
        return None
    created = parts[10].split() if len(parts) > 11 else []
    if len(created) == 2 and created[1].endswith("Z"):
        created = created[1]
    else:
        # 요청 줄에 따옴표가 섞인 드문 줄 — 느린 shlex로 다시 나눈다(21번이 request_creation_time).
        f = shlex.split(line)
        if len(f) < 22:
            return None
        created = f[21]
    target, tgt_t = head[TARGET], head[TGT_T]
    if target == "-" or tgt_t == "-1":
        return epoch(created), None, None
    return epoch(created), target.rsplit(":", 1)[0], float(tgt_t) * 1000


def pct(vals, q):
    """가장 가까운 순위 백분위. vals는 정렬돼 있어야 한다."""
    if not vals:
        return None
    k = max(0, min(len(vals) - 1, int(-(-q * len(vals) // 1)) - 1))
    return vals[k]


def show(v, nd=1):
    return "-" if v is None else f"{v:.{nd}f}"


def slow_pct(vals):
    return None if not vals else 100 * (len(vals) - bisect.bisect_right(vals, SLOW_MS)) / len(vals)


def n_slow(vals):
    return len(vals) - bisect.bisect_right(vals, SLOW_MS)


def pod_names(prom_path):
    """-prom.json alive의 instance("10.0.10.103:9090") → 파드 이름."""
    try:
        prom = json.loads(pathlib.Path(prom_path).read_text())
    except (OSError, ValueError) as e:
        print(f"(prom.json을 읽지 못했다: {e} — 파드 이름 없이 IP로만 찍는다)", file=sys.stderr)
        return {}
    names = {}
    for r in prom.get("alive", {}).get("data", {}).get("result", []):
        m = r.get("metric", {})
        if "instance" in m and "pod" in m:
            names[m["instance"].rsplit(":", 1)[0]] = m["pod"]
    return names


def main():
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    root, start, prom_path = pathlib.Path(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
    max_rps = int(sys.argv[4]) if len(sys.argv) > 4 else 1600
    stages = [r for r in RATES if r <= max_rps]
    end = start + len(stages) * STAGE_S
    offset = start % 60

    # 실행 구간 근처 파일만 읽는다. 파일 이름의 시각은 5분 구간의 끝이다. 접근 로그는 응답 시각 기준이라 앞뒤로 여유를 둔다.
    files = []
    for p in sorted(root.rglob("*.log.gz")):
        m = FILE_TS.search(p.name)
        if not m:
            continue
        t = datetime.datetime.strptime(m.group(1), "%Y%m%dT%H%M").replace(tzinfo=datetime.timezone.utc).timestamp()
        if start - 600 <= t <= end + 600:
            files.append(p)

    first = {}  # 대상 IP → 첫 요청 도착 시각(시작 2분 전부터 — setup 요청 포함)
    ips = []  # IP 목록(인덱스로 저장해 메모리를 줄인다)
    ip_idx = {}
    ts, lat, who = array.array("d"), array.array("d"), array.array("H")
    no_target = collections.Counter()  # 단계 → 대상 응답 없음
    total = 0
    for path in files:
        with gzip.open(path, "rt") as fh:
            for line in fh:
                f = fields(line)
                if f is None:
                    continue
                t, ip, ms = f
                if t < start - FIRST_LOOKBACK_S or t >= end:
                    continue
                if ip is not None and (ip not in first or t < first[ip]):
                    first[ip] = t
                if t < start:
                    continue
                total += 1
                i = int((t - start) // STAGE_S)
                if t - start - i * STAGE_S < RAMP_S:
                    continue
                if ip is None:
                    no_target[i] += 1
                    continue
                if ip not in ip_idx:
                    ip_idx[ip] = len(ips)
                    ips.append(ip)
                ts.append(t)
                lat.append(ms)
                who.append(ip_idx[ip])

    names = pod_names(prom_path)
    # 시작 10초 안에 첫 요청이 있으면 기존 파드다(setup의 링크 생성이 시작 전에 두 파드로 간다).
    is_new = {ip: t0 >= start + 10 for ip, t0 in first.items()}

    # 단계별로 모은다. 키: (단계, 기준, 초) — 기준 "wall"은 벽시계, "k6"는 시작 기준.
    by_sec = collections.defaultdict(list)
    by_sec_new = collections.defaultdict(list)
    stall, rest = collections.defaultdict(list), collections.defaultdict(list)
    stall_np, rest_np = collections.defaultdict(list), collections.defaultdict(list)  # (단계, 새?)
    early = collections.defaultdict(list)  # (단계, 새?) — 벽시계 :00~:10초(리뷰 표본과 같은 창)
    by_min = collections.defaultdict(list)  # (단계, 벽시계 분, 정지 초?) → 처리 시간
    zoom = collections.defaultdict(list)  # (단계, 기준, 칸) → 처리 시간
    allv = collections.defaultdict(list)
    for t, ms, w in zip(ts, lat, who):
        i = int((t - start) // STAGE_S)
        ip = ips[w]
        new = is_new.get(ip, False) and t < first[ip] + NEW_POD_S
        ws, ks = int(t % 60), int((t - start) % 60)
        allv[i].append(ms)
        by_sec[(i, "wall", ws)].append(ms)
        by_sec[(i, "k6", ks)].append(ms)
        by_sec_new[(i, "wall", ws, new)].append(ms)
        if ws in STALL_S or ks in STALL_S:
            stall[i].append(ms)
            stall_np[(i, new)].append(ms)
        elif ws not in CONTROL_S:
            rest[i].append(ms)
            rest_np[(i, new)].append(ms)
        if ws <= 10:
            early[(i, new)].append(ms)
        by_min[(i, int(t // 60) * 60, ws in STALL_S)].append(ms)
        # 확대 창: 가장 가까운 경계(벽시계 정각, k6 시작 + 60k)에서 ±3초. 창 전체가 유지 구간에 든 경계만 쓴다 —
        # 단계 끝 정각(다음 단계 램프 시작)은 앞쪽 3초만 유지 구간이라 칸마다 경계 수가 달라진다.
        hold0, hold1 = start + i * STAGE_S + RAMP_S, start + (i + 1) * STAGE_S
        for basis, zero in (("wall", 0.0), ("k6", float(start % 60))):
            d = (t - zero) % 60
            d = d - 60 if d >= 30 else d
            if -ZOOM_S <= d < ZOOM_S and hold0 <= t - d - ZOOM_S and t - d + ZOOM_S <= hold1:
                zoom[(i, basis, int((d + ZOOM_S) // ZOOM_BIN))].append(ms)
    for d in (by_sec, by_sec_new, stall, rest, stall_np, rest_np, early, by_min, zoom, allv):
        for v in d.values():
            v.sort()

    present = [i for i in range(len(stages)) if allv[i]]
    print(f"## 실행 시작 {datetime.datetime.fromtimestamp(start, datetime.timezone.utc).isoformat()} "
          f"(epoch {start}, 정각 + {offset}초) — 단계 {' '.join(map(str, stages))} rps")
    print()
    print(f"읽은 파일 {len(files)}개, 실행 구간 요청 {total}건(램프 포함), 유지 구간 대상 응답 {len(ts)}건, "
          f"대상 응답 없음 {sum(no_target.values())}건. 시간은 ALB target_processing_time(ms), 도착은 request_creation_time.")
    if offset == 0:
        print("시작이 정각이라 k6 기준 초가 벽시계 초와 같다 — k6 기준 표는 생략한다.")
    print()

    def summary(basis, title):
        print(f"### 요약 — {title} (유지 구간, 정지 = 벽시계 :00~:02 + k6 기준 :00~:02, 정지 밖은 벽시계 :30~:32도 뺀다)")
        print()
        print("| 단계 rps | 요청 | p99 전체 | 정지 p99 | 정지 밖 p99 | :00 p99 | :00 50ms 초과 % | 다른 초 50ms 초과 % 중앙/평균 | 배수 | 재현(≥5배) | 최대 ms | 응답 없음 |")
        print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
        for i in present:
            zero = by_sec[(i, basis, 0)]
            others = [slow_pct(by_sec[(i, basis, s)]) for s in range(1, 60) if by_sec[(i, basis, s)]]
            med = statistics.median(others) if others else None
            mean = statistics.fmean(others) if others else None
            z = slow_pct(zero)
            # 분모는 중앙값이다. 대부분의 초에 느린 요청이 없으면 중앙값이 0이라 평균으로 대신한다(배수가 무한이 되지 않게).
            base = med or mean
            if z is None or base is None:
                ratio, rep = None, "-"
            elif base == 0:
                ratio, rep = None, ("예(다른 초 0)" if z > 0 else "아니오")
            else:
                ratio = z / base
                rep = "예" if ratio >= RATIO else "아니오"
            print(f"| {stages[i]} | {len(allv[i])} | {show(pct(allv[i], .99))} | {show(pct(stall[i], .99))} | "
                  f"{show(pct(rest[i], .99))} | {show(pct(zero, .99))} | {show(z)} | {show(med, 2)}/{show(mean, 2)} | "
                  f"{show(ratio)} | {rep} | {show(allv[i][-1])} | {no_target[i]} |")
        print()

    summary("wall", "벽시계 기준")
    if offset:
        summary("k6", "k6 시작 기준(:00 = 시작 + 60k초)")

    print("### 새 파드 대 기존 파드 (p99 ms, 새 파드 = 실행 중 생긴 파드의 첫 요청 뒤 2분)")
    print()
    print("| 단계 rps | 새 파드 요청 | 정지 p99 새/기존 | 벽시계 :00~:10 p99 새/기존 | 정지 밖 p99 새/기존 |")
    print("| --- | --- | --- | --- | --- |")
    for i in present:
        n_new = len(stall_np[(i, True)]) + len(rest_np[(i, True)])
        print(f"| {stages[i]} | {n_new} | {show(pct(stall_np[(i, True)], .99))}/{show(pct(stall_np[(i, False)], .99))} | "
              f"{show(pct(early[(i, True)], .99))}/{show(pct(early[(i, False)], .99))} | "
              f"{show(pct(rest_np[(i, True)], .99))}/{show(pct(rest_np[(i, False)], .99))} |")
    print()
    print("| 대상 IP | 파드 | 첫 요청(UTC) | 구분 |")
    print("| --- | --- | --- | --- |")
    for ip in sorted(first, key=first.get):
        print(f"| {ip} | {names.get(ip, '?')} | {iso(first[ip])} | {'새' if is_new[ip] else '기존'} |")
    print()

    print("### 분 단위 (유지 구간 안의 온전한 벽시계 분 — CloudWatch TargetResponseTime 대조용)")
    print()
    print("| 단계 rps | 분(UTC) | 요청 | p95 ms | p99 ms | :00~:02 p99 | 나머지 p99 |")
    print("| --- | --- | --- | --- | --- | --- | --- |")
    for i, m in sorted({(i, m) for i, m, _ in by_min}):
        if m < start + i * STAGE_S + RAMP_S or m + 60 > start + (i + 1) * STAGE_S:
            continue
        head, tail = by_min[(i, m, True)], by_min[(i, m, False)]
        v = sorted(head + tail)
        print(f"| {stages[i]} | {iso(m)[:5]} | {len(v)} | {show(pct(v, .95))} | {show(pct(v, .99))} | "
              f"{show(pct(head, .99))} | {show(pct(tail, .99))} |")
    print()

    def per_second(basis, title):
        print(f"### 초별 p99 ms / 50ms 초과 건수 — {title}")
        print()
        print("| 초 | " + " | ".join(str(stages[i]) for i in present) + " |")
        print("| --- | " + " | ".join("---" for _ in present) + " |")
        for s in range(60):
            cells = []
            for i in present:
                v = by_sec[(i, basis, s)]
                cells.append(f"{show(pct(v, .99))}/{n_slow(v)}" if v else "-")
            print(f"| :{s:02d} | " + " | ".join(cells) + " |")
        print()

    per_second("wall", "벽시계 초")
    if offset:
        per_second("k6", "k6 시작 기준 초")

    new_stages = [i for i in present if any(by_sec_new[(i, "wall", s, True)] for s in range(60))]
    if new_stages:
        print("### 초별 p99 ms 새 파드/기존 파드 — 벽시계 초 (새 파드가 있던 단계만)")
        print()
        print("| 초 | " + " | ".join(str(stages[i]) for i in new_stages) + " |")
        print("| --- | " + " | ".join("---" for _ in new_stages) + " |")
        for s in range(60):
            cells = [f"{show(pct(by_sec_new[(i, 'wall', s, True)], .99))}/{show(pct(by_sec_new[(i, 'wall', s, False)], .99))}"
                     for i in new_stages]
            print(f"| :{s:02d} | " + " | ".join(cells) + " |")
        print()

    def zoomed(basis, title):
        # 칸마다 도착 수를 그 단계 유지 구간의 경계 개수로 나눈다(경계 하나당 도착 수).
        print(f"### 정지 앞뒤 ±{ZOOM_S}초, {int(ZOOM_BIN * 1000)}ms 칸 — {title} (경계당 도착 수 · p99 ms)")
        print()
        print("| 경계 기준 초 | " + " | ".join(str(stages[i]) for i in present) + " |")
        print("| --- | " + " | ".join("---" for _ in present) + " |")
        zero = 0 if basis == "wall" else start % 60
        bounds = {i: sum(1 for b in range(start + i * STAGE_S + RAMP_S, start + (i + 1) * STAGE_S)
                         if (b - zero) % 60 == 0 and b - ZOOM_S >= start + i * STAGE_S + RAMP_S
                         and b + ZOOM_S <= start + (i + 1) * STAGE_S) for i in present}
        for k in range(int(2 * ZOOM_S / ZOOM_BIN)):
            cells = []
            for i in present:
                v = zoom[(i, basis, k)]
                cells.append(f"{len(v) / bounds[i]:.0f} · {show(pct(v, .99))}" if v and bounds[i] else "-")
            print(f"| {k * ZOOM_BIN - ZOOM_S:+.1f} | " + " | ".join(cells) + " |")
        print()
        print("창 전체(±3초)가 유지 구간에 든 경계만 쓴다(정각 시작이면 단계마다 2개). 낮은 단계의 칸은 표본이 적다.")
        print()

    zoomed("wall", "벽시계 정각")
    if offset:
        zoomed("k6", "k6 시작 + 60k초")


if __name__ == "__main__":
    main()
