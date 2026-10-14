#!/usr/bin/env python3
"""ALB 접근 로그에서 실패 요청을 분류한다(plan 0015 판정 (f) — 502·타임아웃).

사용: python3 -I load/k6/alb-errors.py <로그 디렉터리>   (aws s3 sync로 받은 디렉터리, *.log.gz를 모두 읽는다)

고르는 줄: ALB 상태 5xx, 460(클라이언트가 먼저 끊음 — k6 60초 타임아웃), 대상 상태가 "-"(대상이 응답하지 않음).
묶는 기준: ALB 상태 · 대상 상태 · error_reason · classification. 묶음마다 건수, 초(0~59) 분포, 대상(파드) IP,
처리 시간(request/target/response, 초)을 찍는다. 클라이언트 IP는 측정 집 IP라 읽기만 하고 찍지 않는다.
필드 순서는 ELB 사용자 가이드 "Access log entries"(Application Load Balancer)를 따른다. 로컬 파일만 읽는다.
"""
import collections
import gzip
import pathlib
import shlex
import statistics
import sys

# 필드 위치(0부터).
TIME, TARGET, REQ_T, TGT_T, RESP_T, ELB_CODE, TGT_CODE = 1, 4, 5, 6, 7, 8, 9
ERROR_REASON, CLASSIFICATION = 24, 27


def pick(f):
    elb, tgt = f[ELB_CODE], f[TGT_CODE]
    return elb.startswith("5") or elb == "460" or tgt == "-"


def main():
    root = pathlib.Path(sys.argv[1])
    total = 0
    groups = collections.defaultdict(list)
    for path in sorted(root.rglob("*.log.gz")):
        with gzip.open(path, "rt") as fh:
            for line in fh:
                # 앞 12개 필드에는 공백·따옴표가 없다 — 실행 하나가 수백만 줄이라 고를 때는 빠른 split만 쓰고,
                # 고른 줄만 shlex로 끝까지 나눈다(따옴표 안의 요청·User-Agent에 공백이 있다).
                head = line.split(" ", 12)
                if len(head) < 12:
                    continue
                total += 1
                if not pick(head):
                    continue
                f = shlex.split(line)
                if len(f) <= CLASSIFICATION:
                    continue
                key = (f[ELB_CODE], f[TGT_CODE], f[ERROR_REASON], f[CLASSIFICATION])
                groups[key].append(f)
    print(f"전체 요청 {total}건, 고른 줄 {sum(len(v) for v in groups.values())}건")
    print("ALB 5xx 합 " + str(sum(len(v) for k, v in groups.items() if k[0].startswith("5"))) + "건, 그중 502 "
          + str(sum(len(v) for k, v in groups.items() if k[0] == "502")) + "건")
    for (elb, tgt, reason, cls), rows in sorted(groups.items(), key=lambda kv: -len(kv[1])):
        print()
        print(f"## ALB {elb} · 대상 {tgt} · error_reason {reason or '-'} · classification {cls or '-'} — {len(rows)}건")
        seconds = collections.Counter(int(r[TIME][17:19]) for r in rows)
        print("초 분포(초:건수): " + ", ".join(f"{s:02d}:{n}" for s, n in sorted(seconds.items())))
        targets = collections.Counter(r[TARGET].rsplit(":", 1)[0] for r in rows)
        print("대상 IP: " + ", ".join(f"{t} {n}" for t, n in targets.most_common()))
        for name, idx in (("request", REQ_T), ("target", TGT_T), ("response", RESP_T)):
            vals = [float(r[idx]) for r in rows if r[idx] != "-1"]
            if vals:
                print(f"{name} 처리 시간 s: 최소 {min(vals):.3f} · 중앙 {statistics.median(vals):.3f} · 최대 {max(vals):.3f}")
            else:
                print(f"{name} 처리 시간: 전부 -1(그 단계에 가지 못했다)")
        print("예시 시각: " + ", ".join(r[TIME] for r in rows[:3]))


if __name__ == "__main__":
    main()
