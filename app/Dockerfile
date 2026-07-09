# linkpulse 앱 이미지 — 멀티스테이지 빌드.
# 빌드 스테이지에서 정적 바이너리를 만들고, 런타임은 distroless(static)로 최소화한다.

# --- 빌드 스테이지 ---
# 빌드 스테이지는 러너의 네이티브 아키텍처($BUILDPLATFORM)에서 돌리고, GOARCH로 타깃(arm64)을
# 크로스컴파일한다. CGO 비활성 순수 Go라 x86 러너에서도 QEMU 없이 네이티브 속도로 arm64를 만든다
# → 배포가 불안정한 arm64 preview 러너에 의존하지 않는다(ADR 0001).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# buildx가 --platform으로 주입하는 타깃 아키텍처. 옵션 없는 일반 docker build 시 호스트값이 들어온다.
ARG TARGETARCH

# 의존성을 먼저 받아 레이어 캐시를 활용한다(소스만 바뀌면 재다운로드 안 함).
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO 비활성 정적 바이너리 → distroless/static·scratch와 호환된다. GOARCH로 타깃 아키텍처 크로스컴파일.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -o /out/server ./cmd/server

# --- 런타임 스테이지 ---
# distroless static: 셸·패키지 매니저 없는 최소 런타임. nonroot 사용자로 실행한다.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
