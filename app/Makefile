# linkpulse /app 개발용 Makefile.
# 로컬 개발 편의를 위한 단축 명령 모음.

.PHONY: run build test tidy vet

run: ## 로컬에서 서버 실행
	go run ./cmd/server

build: ## 정적 바이너리 빌드 (distroless 런타임과 호환되도록 CGO 비활성화)
	CGO_ENABLED=0 go build -o bin/server ./cmd/server

# -race를 붙이는 이유: dbcreds의 비밀번호·세대·backoff 상태와 detached refresh goroutine을
# 여러 Connect가 동시에 읽고 쓴다. mutex·singleflight 경계의 회귀는 레이스 검출기가 가장 잘 잡는다.
test: ## 단위 테스트 실행 (레이스 검출 포함)
	go test -race ./...

tidy: ## go.mod / go.sum 정리
	go mod tidy

vet: ## 정적 분석
	go vet ./...
