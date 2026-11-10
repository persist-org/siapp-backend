#!/usr/bin/env python3
"""Make api/openapi.dev.yaml for the dev Swagger page.

It is api/openapi.yaml cut down to the operations the API really serves (api/built-routes.txt),
with one same-origin server (/api/v1), so "Try it out" works behind the dev nginx. Schemas,
parameters and responses nothing refers to any more are dropped, so the page shows only what is
used. api/openapi.yaml itself is never changed: it stays the complete contract.

    python3 scripts/make-dev-spec.py            # from the project folder

Needs PyYAML (pip install pyyaml). Run it again, and commit the result, whenever api/openapi.yaml
or api/built-routes.txt changes.
"""
import re
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "api" / "openapi.yaml"
ROUTES = ROOT / "api" / "built-routes.txt"
OUT = ROOT / "api" / "openapi.dev.yaml"
METHODS = ("get", "post", "put", "patch", "delete", "head", "options")
PREFIX = "/api/v1"
PRUNABLE = ("schemas", "parameters", "responses", "requestBodies", "headers", "examples", "links", "callbacks")


def norm(path):
    return re.sub(r"\{[^}]*\}", "{}", path)


def built_routes():
    out = set()
    for line in ROUTES.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#"):
            method, route = line.split(" ", 1)
            out.add((method.upper(), norm(route)))
    return out


def refs_in(node, found):
    if isinstance(node, dict):
        ref = node.get("$ref")
        if isinstance(ref, str) and ref.startswith("#/"):
            found.add(ref)
        for value in node.values():
            refs_in(value, found)
    elif isinstance(node, list):
        for value in node:
            refs_in(value, found)


def resolve(spec, ref):
    cur = spec
    for part in ref[2:].split("/"):
        cur = cur[part.replace("~1", "/").replace("~0", "~")]
    return cur


def main():
    spec = yaml.safe_load(SRC.read_text())
    built = built_routes()

    kept_ops = dropped_ops = 0
    paths = {}
    for path, item in spec["paths"].items():
        keep = {k: v for k, v in item.items() if k not in METHODS}
        ops = 0
        for method in METHODS:
            if method not in item:
                continue
            if (method.upper(), norm(PREFIX + path)) in built:
                keep[method] = item[method]
                ops += 1
            else:
                dropped_ops += 1
        if ops:
            paths[path] = keep
            kept_ops += ops
    spec["paths"] = paths

    # One server, same origin as the page.
    spec["servers"] = [{"url": "/api/v1", "description": "Dev server (same origin as this page)"}]

    # Tags nothing uses any more.
    used_tags = {t for item in paths.values() for m in METHODS if m in item for t in item[m].get("tags", [])}
    if "tags" in spec:
        spec["tags"] = [t for t in spec["tags"] if t.get("name") in used_tags]
    spec.pop("x-tagGroups", None)

    # Components nothing refers to any more (transitively).
    comp = spec.get("components", {})
    roots = {"paths": spec["paths"], "security": spec.get("security"), "webhooks": spec.get("webhooks")}
    seen, todo = set(), set()
    refs_in(roots, todo)
    while todo:
        ref = todo.pop()
        if ref in seen:
            continue
        seen.add(ref)
        try:
            refs_in(resolve(spec, ref), todo)
        except (KeyError, TypeError):
            print("warning: unresolved reference", ref, file=sys.stderr)
    for kind in PRUNABLE:
        if kind in comp:
            comp[kind] = {name: v for name, v in comp[kind].items() if f"#/components/{kind}/{name}" in seen}
            if not comp[kind]:
                del comp[kind]

    info = spec.setdefault("info", {})
    info["title"] = f"{info.get('title', 'API')} (dev: built endpoints only)"
    info["description"] = (
        "**Dev copy.** Only the operations the API serves today. The complete contract is api/openapi.yaml; "
        "this file is made by scripts/make-dev-spec.py.\n\n" + (info.get("description") or "")
    )

    def literal(dumper, data):
        style = "|" if "\n" in data else None
        return dumper.represent_scalar("tag:yaml.org,2002:str", data, style=style)

    yaml.add_representer(str, literal, Dumper=yaml.SafeDumper)
    OUT.write_text(yaml.dump(spec, Dumper=yaml.SafeDumper, sort_keys=False, allow_unicode=True, width=120))
    print(f"kept {kept_ops} operations, dropped {dropped_ops}; wrote {OUT.relative_to(ROOT)}")
    unmatched = built - {(m.upper(), norm(PREFIX + p)) for p, it in yaml.safe_load(SRC.read_text())["paths"].items()
                         for m in METHODS if m in it}
    if unmatched:
        print("routes in built-routes.txt but not in the spec (e.g. /health, /ws):", len(unmatched))


if __name__ == "__main__":
    main()
