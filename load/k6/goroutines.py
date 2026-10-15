#!/usr/bin/env python3
"""고루틴 스냅숏(goroutine?debug=2)에서 처리 중인 요청이 어디서 기다리는지 센다(plan 0016 설계 4).

사용: python3 -I load/k6/goroutines.py <스냅숏 파일>...   (load/k6/goroutine-snap.sh가 남긴 <접두>-gr-<epoch>.txt)

처리 중인 요청 = 스택에 net/http.serverHandler.ServeHTTP(핸들러 안) 또는 net/http.(*response).finishRequest(핸들러가 끝나고
응답을 비우는 중)가 있는 고루틴. keep-alive로 다음 요청을 기다리는 연결은 빠진다.
내부 포트(9090)의 pprof·지표 고루틴은 뺀다 — 스냅숏을 만드는 고루틴 자신과 그때의 Prometheus 긁기가 "실행 대기"를 부풀리지 않게.
분류(위에서부터 처음 맞는 것):
  실행 대기   상태가 runnable·running — CPU를 기다린다(H1 CFS 스로틀·GOMAXPROCS)
  DB 연결 대기 database/sql.(*DB).conn — 풀이 비었다
  DB 응답 대기 스택에 pgx가 있다 — RDS 응답·네트워크(S)
  로그 쓰기   스택에 log/slog가 있다 — 핸들러 뮤텍스·stdout 쓰기(H2)
  응답 쓰기   net/http 응답 쓰기·플러시(finishRequest 포함) — ALB 쪽 연결
  기타        맨 위 함수 이름을 함께 센다
처리 중 요청 수가 대조(:30.2)보다 확 줄면 요청이 파드에 닿기 전(ALB·네트워크)에서 멈춘 것이다.
로컬 파일만 읽는다.
"""
import collections
import pathlib
import re
import sys

HEADER = re.compile(r"^goroutine \d+ \[([^,\]]+)")


def goroutines(text):
    """(상태, 함수 이름 목록) — 함수 이름은 스택 위(가장 최근 호출)부터다. 파일 경로 줄은 버린다."""
    for block in text.strip().split("\n\n"):
        lines = block.splitlines()
        m = HEADER.match(lines[0]) if lines else None
        if not m:
            continue
        funcs = [ln for ln in lines[1:] if not ln.startswith("\t")]
        yield m.group(1), funcs


def classify(state, funcs):
    stack = "\n".join(funcs)
    if state in ("runnable", "running"):
        return "실행 대기"
    if "database/sql.(*DB).conn(" in stack:
        return "DB 연결 대기"
    if "github.com/jackc/pgx" in stack:
        return "DB 응답 대기"
    if "log/slog." in stack:
        return "로그 쓰기"
    if "net/http.(*response)" in stack or "net/http.(*chunkWriter)" in stack or "bufio.(*Writer).Flush" in stack:  # finishRequest 포함
        return "응답 쓰기"
    return "기타"


CLASSES = ["실행 대기", "DB 연결 대기", "DB 응답 대기", "로그 쓰기", "응답 쓰기", "기타"]


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    print("| 파일 | 고루틴 | 처리 중 요청 | " + " | ".join(CLASSES) + " | 기타 맨 위 함수(상위 3) |")
    print("| --- | --- | --- | " + " | ".join("---" for _ in CLASSES) + " | --- |")
    for path in sys.argv[1:]:
        text = pathlib.Path(path).read_text(errors="replace")
        total, counts, other = 0, collections.Counter(), collections.Counter()
        for state, funcs in goroutines(text):
            total += 1
            if not any(f.startswith(("net/http.serverHandler.ServeHTTP(", "net/http.(*response).finishRequest(")) for f in funcs):
                continue
            if any("net/http/pprof." in f or "promhttp." in f for f in funcs):
                continue
            c = classify(state, funcs)
            counts[c] += 1
            if c == "기타":
                other[f"{re.sub(r'[(][^()]*[)]$', '', funcs[0])} [{state}]"] += 1
        busy = sum(counts.values())
        top = ", ".join(f"{k} {n}" for k, n in other.most_common(3)) or "-"
        print(f"| {pathlib.Path(path).name} | {total} | {busy} | " + " | ".join(str(counts[c]) for c in CLASSES) + f" | {top} |")


if __name__ == "__main__":
    main()
