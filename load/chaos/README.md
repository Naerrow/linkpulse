# chaos-healthz — GameDay 장애 주입 이미지

P3-2 GameDay(S1)에서 **자동 방어(deployment circuit breaker 롤백)를 실증**하기 위한 이미지.
계획·예측표: [`docs/plans/0003-p3-2-fault-injection.md`](../../docs/plans/0003-p3-2-fault-injection.md) / 기록지: [`docs/postmortems/2026-07-06-gameday-01.md`](../../docs/postmortems/2026-07-06-gameday-01.md)

- **동작**: busybox `httpd`가 8080에서 정적 파일만 서빙. `GET /` 200, **`GET /healthz` 404**.
  프로세스는 죽지 않으므로 "크래시"가 아니라 **헬스체크 실패** 유형의 배포 실패를 만든다.
- **안전**: taskdef의 env/secrets(DB_PASSWORD 등)가 주입되지만 이 이미지는 아무것도 읽지 않는다
  (busybox httpd, DB 무접촉). RDS·데이터에 영향 없음.

> **드릴 전용이다.** 평상시 배포 금지. 배포(주입)는 사람이 GameDay 절차에 따라 `deploy.yml` > `workflow_dispatch`(`image_tag=chaos-healthz-v1`)로만 실행한다(가드레일 #1 — push·배포는 사람).

## GameDay 주입 전 체크 (2026-07-06 드릴 교훈 반영)

1. **관측 먼저 켠다(C-1)** — S1 dispatch **전에** healthz 폴링을 백그라운드로 띄운다(무중단 P1의 정량 근거. 1차 드릴은 이게 없어 무중단을 증명 못 했다 — 회고 A-4).
   ```bash
   while true; do echo "$(date '+%H:%M:%S') $(curl -sk -o /dev/null -w '%{http_code}' --max-time 4 https://lpulse.live/healthz)"; sleep 5; done
   ```
2. **진행 중 run이 없는지 확인 후 dispatch** — 중복 주입 방지(1차 드릴에서 S1이 원인 불명으로 두 번 실행됐다 — 회고 A-7).
   ```bash
   gh run list --workflow=deploy.yml --branch main -L 3   # in_progress 없어야 함
   gh workflow run deploy.yml --ref main -f image_tag=chaos-healthz-v1
   gh run list --workflow=deploy.yml --branch main --event workflow_dispatch -L 5 \
     --json databaseId,createdAt,status,url   # createdAt이 dispatch 시각 이후인 run이 방금 것 — id 고정(직전 dispatch 오인 방지)
   ```

## 빌드 → 로컬 검증 → push → 아키텍처 확인 (전부 사람 실행)

taskdef가 **ARM64**(Graviton)라 `--platform linux/arm64` 명시가 필수다. 아래 4단계를 순서대로
전부 통과해야 Step 1 완료 조건 충족(계획 §5 — ARM64 리스크 이중 차단).

```bash
# 0) 변수 준비 (ECR URL은 조회로 — 계정 ID 하드코딩 회피)
ECR_URL=$(aws ecr describe-repositories --repository-names linkpulse/app \
  --query 'repositories[0].repositoryUri' --output text --region ap-northeast-2)

# 1) arm64 명시 빌드 (로컬 검증용 --load)
docker buildx build --platform linux/arm64 -t chaos-healthz:local --load load/chaos

# 2) 로컬 검증: / 는 200, /healthz 는 404 (프로세스는 계속 살아 있어야 한다)
docker run --rm -d --name chaos-healthz -p 8080:8080 chaos-healthz:local
curl -i http://localhost:8080/          # HTTP/1.0 200 OK 기대
curl -i http://localhost:8080/healthz   # HTTP/1.0 404 Not Found 기대
docker ps --filter name=chaos-healthz   # 두 curl 후에도 Up 상태여야 함
docker stop chaos-healthz

# 3) ECR 로그인 + push (같은 앱 리포지토리에 태그만 다르게 — deploy.yml preflight가 이 리포를 본다)
aws ecr get-login-password --region ap-northeast-2 \
  | docker login --username AWS --password-stdin "${ECR_URL%%/*}"
docker buildx build --platform linux/arm64 -t "${ECR_URL}:chaos-healthz-v1" --push load/chaos

# 4) push된 매니페스트의 아키텍처가 arm64인지 확인 (완료 조건)
docker buildx imagetools inspect "${ECR_URL}:chaos-healthz-v1"
# → Platform: linux/arm64 확인
```

## 함정·수명 주기

- **태그 규칙**: `chaos-healthz-v1`은 deploy.yml의 태그 검증 `^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$` 통과.
  ECR은 MUTABLE이므로 재push 시 덮어써진다(드릴 이미지라 무방).
- **ECR lifecycle이 지울 수 있다**: 리포지토리는 최근 30개만 보관(`ecr.tf`). CI 배포가 쌓이면
  이 태그도 만료될 수 있다 — 드릴 직전 `aws ecr describe-images --repository-name linkpulse/app
--image-ids imageTag=chaos-healthz-v1 --region ap-northeast-2`로 존재 확인, 없으면 재push.
- **full destroy 후엔 사라진다**: `scripts/full-destroy-prod.sh`는 ECR 이미지까지 지운다.
  재생성(full apply) 후 드릴하려면 이 README 절차로 재push.
- **주입 시 CI(checks)는 도는 게 아니라 생략된다**: `image_tag` 입력 자체로 `build_image=false`
  → checks skip. ECR에 태그가 실제 있는지는 별개의 preflight 게이트가 확인한다(없으면 job fail).
