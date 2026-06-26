provider "aws" {
  region = "ap-northeast-2"

  # 이 스택이 만드는 모든 리소스에 공통 태그를 단다.
  default_tags {
    tags = {
      Project     = "linkpulse"
      Environment = "bootstrap"
      ManagedBy   = "terraform"
    }
  }
}
