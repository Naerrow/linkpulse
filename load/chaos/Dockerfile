# chaos-healthz — P3-2 GameDay S1 주입용 이미지 (docs/plans/0003-p3-2-fault-injection.md)
#
# 의도: 8080 리슨은 정상(프로세스 계속 생존)이지만 /healthz가 404
#       → 크래시 루프가 아니라 "떠 있는데 ALB 헬스체크만 실패"하는 장애 유형을 만든다.
#       taskdef(ecs.tf)에 command/entryPoint 오버라이드와 컨테이너 레벨 healthCheck가
#       없음을 확인했으므로, 이 CMD가 그대로 실행되고 판정은 ALB 타깃그룹 헬스체크만 한다.
#
# 주의: taskdef가 ARM64(Graviton)이므로 반드시 --platform linux/arm64로 빌드한다.
#       불일치 시 exec format error 크래시가 되어 "의도와 다른" 장애 유형이 된다(README 참고).
FROM busybox:1.37

# 문서 루트에 index.html만 둔다 — GET / 는 200(프로세스 생존 확인용),
# GET /healthz 는 파일이 없어 404(주입 목적. ALB matcher는 200만 healthy).
RUN mkdir -p /www && printf 'chaos-healthz-v1: /healthz intentionally missing\n' > /www/index.html

EXPOSE 8080

# -f 포그라운드(PID 1 유지), -v 요청 로그를 stdout으로(awslogs로 ALB 헬스체크 404가 보인다),
# -p 8080 은 앱과 동일 포트(taskdef portMappings 8080).
CMD ["httpd", "-f", "-v", "-p", "8080", "-h", "/www"]
