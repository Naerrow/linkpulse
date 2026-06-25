# linkpulse 앱 이미지 — 멀티스테이지 빌드.
# 빌드 스테이지에서 정적 바이너리를 만들고, 런타임은 distroless(static)로 최소화한다.

# --- 빌드 스테이지 ---
FROM golang:1.26-alpine AS build
WORKDIR /src

# 의존성을 먼저 받아 레이어 캐시를 활용한다(소스만 바뀌면 재다운로드 안 함).
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO 비활성 정적 바이너리 → distroless/static·scratch와 호환된다.
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/server ./cmd/server

# --- 런타임 스테이지 ---
# distroless static: 셸·패키지 매니저 없는 최소 런타임. nonroot 사용자로 실행한다.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
