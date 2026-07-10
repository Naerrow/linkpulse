# 회고 — 2026-07-09 배포 러너 미획득 (GitHub Actions 플랫폼 인시던트)

- 날짜(발생): 2026-07-09
- 심각도: 실배포 장애 · 사용자 영향 없음 (드릴 아님)
- 상태: 확정
- 관련: run `28994311497`(실패)·`28995170473`(성공), PR #8(x86+buildx)·#9(단일 job), ADR 0001, 메모리 `deploy-arm-runner-flake`

## 1. 한 줄 요약

P4 트리오 머지 배포가 **GitHub Actions 플랫폼 인시던트("Delays starting Actions runs")로 러너를 배정받지 못해** 실패했다. 앱 코드·워크플로 결함이 아니다. 처음엔 "ARM preview 러너 특유 문제"로 좁게 진단해 배포 빌드를 x86 GA 러너 + buildx 크로스빌드로 옮겼으나(PR #8), **x86 GA에서도 같은 미획득이 재발**하면서 원인이 러너 종류를 안 가리는 플랫폼 장애임이 확정됐다. 최종 완화로 배포 워크플로를 **3-job → 단일 job**으로 합쳐(PR #9) 배포 1회당 러너 배정 횟수를 3→1로 줄여 실패 노출을 축소했다. 플랫폼 장애 자체는 못 막으므로, 대응은 여전히 **githubstatus 확인 → 인시던트 해소 후 재실행**이다.

## 2. 증상

- **1차** (PR #7 머지, push 배포, run `28994311497`): `deploy` job(당시 `ubuntu-24.04-arm`)이 ~15분 뒤 실패.
  - `The job was not acquired by Runner of type hosted even after multiple attempts` + `Internal server error`.
  - 같은 run의 setup·checks(x86)는 통과, ARM job만 실패 → 처음엔 "ARM 러너 특유"로 오인.
- **2차** (PR #8 = ARM→x86 fix 머지 배포): 이번엔 **`checks` job이 `ubuntu-latest`(x86 GA)에서 미획득**으로 실패.
  - x86 GA에서도 같은 실패 = **러너 종류 무관하다는 결정적 증거.**
- 공통점: 실패 지점이 빌드/배포 스텝이 아니라 그 **이전 '러너 확보' 단계**. 스텝은 하나도 실행되지 않았다.

## 3. 근본 원인 — 진단을 한 번 정정했다 (정직하게)

1. **1차 진단 (좁고, 부분적으로 틀림):** 배포가 GitHub의 가장 불안정한 러너(`ubuntu-24.04-arm`, public preview)에 단일 의존. → x86 GA로 옮기면 해결될 것이라 봄.
2. **재발이 준 정정 (실제 주원인):** `githubstatus.com` 실측 = **Actions 컴포넌트 "Delays starting Actions runs" 인시던트**(DEGRADED_PERFORMANCE, 04:34:24Z 시작). 러너 종류를 안 가리며, 그래서 x86 GA로 옮긴 뒤에도 재발했다. → **preview 러너는 악화 요인이었을 뿐, 그날 실패의 지배적 원인은 플랫폼 장애.**
3. **우리가 줄일 수 있는 구조적 기여:** 배포 워크플로가 `setup → checks → deploy` **3-job**이라, 배포 1회에 GitHub-hosted 러너를 **3번** 배정받아야 했다. 러너 배정 장애 창에서는 셋 중 하나만 못 잡아도 배포 전체가 실패 → **실패 노출이 3배.**

> 핵심 교훈: `job not acquired`는 러너 특성 문제로 단정하기 전에 **githubstatus.com부터** 본다(플랫폼 장애 vs 러너 종류 구분).

## 4. "왜 하필 이때" — 배포 내용은 무관 (증거)

- PR #7이 바꾼 파일: `app/*`, `ecs.tf`, plan, `.gitignore` — `deploy.yml` 미포함.
- `deploy.yml`은 P2 이후 러너 라벨 불변. 그날 실패한 파이프라인 = 이전에 성공하던 것과 바이트 동일.
- 러너 확보는 스텝 실행 **전**, 오직 `runs-on` 라벨만 보고 GitHub이 결정 → 배포 코드 내용이 개입할 경로가 물리적으로 없다.
- 인시던트 시작 04:34:24Z → 10초 뒤 PR #7 배포 실패(04:34:34Z). 24분 뒤 동일 파이프라인 재시도(04:58Z) → 성공(간헐 회복창). = 유일한 변수는 **그 순간 GitHub 러너 가용성.**

## 5. 영향 — 없음

- 러너 미확보 = 스텝 미실행 → 이미지 빌드·ECR 푸시·taskdef 등록·`update-service` **전부 미발생**. 부분 배포 없음, taskdef 리비전 0개 생성.
- 서비스는 직전 리비전 유지. (리비전 흐름: rev15(P3) → **rev16 = terraform apply**(00:44Z / 09:44 KST, image `:v1`, `APP_ENV=production` 최초 주입) → **rev17 = 재시도 성공 배포**(04:58Z, P4 코드). 실패 run들은 리비전 0 기여.)

## 6. 대응 · 타임라인 (UTC)

| 시각(Z) | 이벤트 | 출처 |
| --- | --- | --- |
| 00:44 | terraform apply → taskdef rev16 (APP_ENV 주입) | (KST 09:44) |
| 04:34:24 | GitHub Actions 인시던트 "Delays starting Actions runs" 시작 | githubstatus.com |
| 04:34:34 | PR #7 push 배포 실패 (ARM `deploy` job 미획득) | run `28994311497` |
| 04:58 | `workflow_dispatch` 재배포 성공 → taskdef rev17 | run `28995170473` |
| ~06:00 | PR #8(x86 fix) 배포 실패 (`checks` job, x86 GA 미획득) | 인시던트 지속 |

- 즉시 대응 = **재실행**(`gh run rerun --failed` 또는 `workflow_dispatch`). 단 이는 **증상 대응** — 인시던트 해소 전엔 재발한다.
- 근본 예방은 불가(GitHub 플랫폼 장애). 우리가 통제 가능한 건 **노출 축소**뿐 → §7.

## 7. 두 번의 수정 — 각각이 '실제로' 막는 것

| 수정 | 내용 | 막는 것 | 못 막는 것 |
| --- | --- | --- | --- |
| **PR #8** `3bbdf49` | `ubuntu-24.04-arm` → `ubuntu-latest`(x86 GA) + `docker buildx --platform linux/arm64` 크로스빌드. Dockerfile 빌드 스테이지 `--platform=$BUILDPLATFORM` + `GOARCH`(CGO 비활성 순수 Go라 QEMU 불필요). | preview 러너 의존 · "ARM preview 미획득" 실패 클래스 | 러너 종류 무관 플랫폼 장애(재발이 증명) |
| **PR #9** `783f325` | `setup→checks→deploy` 3-job → **단일 `deploy` job**. 검증·빌드·배포를 한 러너에서 이어감(롤백 시 검증 스텝 skip). | 러너 배정 3회 → **1회**, 우리 쪽 실패 노출 ~3배 축소 | 플랫폼 장애로 그 1회 배정마저 실패하는 경우 |

- 두 수정 모두 **유효한 개선**이되, 어느 것도 플랫폼 전면 장애를 없애진 못한다. PR #8은 평상시 신뢰성(GA ≫ preview), PR #9는 장애창에서의 노출 면적을 줄인다.
- Fargate 런타임은 arm64(Graviton) 그대로 → 비용 이점 유지.
- **상태 note:** buildx arm64 크로스빌드는 로컬 실측(`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build` → `ELF ARM aarch64, statically linked`)까지 확인. **CI에서의 첫 실제 실행은 러너가 정상 배정된 배포에서 확인**(인시던트 지속 중엔 미실행) — 최근 배포 run으로 사용자 확인 요망.

## 8. 정직한 한계

- x86 GA도 결국 GitHub → **전면 Actions 장애**는 못 막는다. 완전 독립은 self-hosted 러너뿐(P5 홈랩 k3s 선택지, 현 규모엔 과함).
- 단일 job 트레이드오프: job 단위 병렬/재사용은 포기(배포 경로엔 무관), 대신 로그가 한 곳에 모여 가독성은 오히려 낫다.

## 9. 교훈

- 프로덕션 크리티컬 경로(배포)에 preview/비-GA 컴포넌트를 단일 의존하지 말 것.
- 크로스컴파일 가능한 스택(순수 Go 등)은 타깃 아키텍처 러너에 의존하지 말고 GA 러너에서 크로스빌드.
- 러너 배정은 job 수만큼 반복된다 → 크리티컬 경로의 job 수를 줄이면 배정 실패 노출이 준다.
- `job not acquired`는 **githubstatus.com부터**. "재시도로 해결"은 증상 대응 — 통제 가능한 구조적 기여(의존·배정 횟수)를 제거한다.
- 좁았던 첫 진단을 데이터(재발)로 정정한 과정을 기록에 남긴다(다음에 같은 함정 회피).

## 10. 참고

- 실패: run `28994311497`(push, 04:34:34Z) · PR #8 배포(`checks` job, x86, ~06:00Z)
- 성공: run `28995170473`(workflow_dispatch, 04:58Z) → taskdef rev17
- terraform apply: taskdef rev16 (00:44Z / 09:44 KST, image `:v1`, `APP_ENV=production`)
- 커밋: PR #8 `3bbdf49`(x86+buildx) · PR #9 `783f325`(단일 job)
- 관련 문서: ADR `0001`(배포 경계·arm64 정경로), 메모리 `deploy-arm-runner-flake`(플랫폼 장애 정정)
