#!/usr/bin/env python3
"""CRD(YAML)의 openAPIV3Schema를 kubeconform용 JSON 스키마로 바꾼다(scripts/check-charts.sh).

API 서버는 CRD에 없는 필드를 오류 없이 버린다. 그래서 오타가 조용히 무시된다. 검사에서는 그것을 잡으려고
properties가 있는 모든 객체에 additionalProperties: false를 넣는다(kubeconform openapi2jsonschema의 strict 변환과 같다).
x-kubernetes-preserve-unknown-fields가 있는 객체는 건드리지 않는다.

사용: crd-to-jsonschema.py <crd.yaml> <버전> <출력.json>
helm 템플릿 줄({{ ... }})은 지우고 읽는다(차트 templates/ 안의 CRD를 그대로 넘길 수 있게).
"""
import json
import sys

import yaml


def deny_additional(node):
    if isinstance(node, dict):
        if (
            node.get("type") == "object"
            and "properties" in node
            and "additionalProperties" not in node
            and not node.get("x-kubernetes-preserve-unknown-fields")
        ):
            node["additionalProperties"] = False
        for v in node.values():
            deny_additional(v)
    elif isinstance(node, list):
        for v in node:
            deny_additional(v)


def main():
    src, version, out = sys.argv[1:4]
    text = "\n".join(l for l in open(src).read().split("\n") if not l.strip().startswith("{{"))
    crd = yaml.safe_load(text)
    schema = next(v for v in crd["spec"]["versions"] if v["name"] == version)["schema"]["openAPIV3Schema"]
    deny_additional(schema)
    with open(out, "w") as f:
        json.dump(schema, f, indent=1, ensure_ascii=True)
        f.write("\n")


if __name__ == "__main__":
    main()
