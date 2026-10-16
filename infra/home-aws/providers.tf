provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project     = var.project
      Environment = "home"
      ManagedBy   = "terraform"
    }
  }
}
