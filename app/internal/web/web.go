// web 패키지는 화면 한 장(html·js·css)을 바이너리에 내장한다(plan 0011).
// 빌드 도구·외부 CDN·프레임워크가 없어서 이미지 하나로 배포가 끝나고 인프라·비용이 늘지 않는다.
package web

import "embed"

// Files는 화면 파일 묶음이다. httpapi가 GET /와 GET /static/{file}로 서빙한다.
//
//go:embed index.html app.js style.css
var Files embed.FS
