#!/usr/bin/env python3
"""
Validate example messages against the protocol schemas.

One entry point: every example is checked against envelope.schema.json, which
binds `type` to the right body schema itself. Nothing here knows which body
belongs to which type — that knowledge lives in the envelope and in one place.
"""
import json, sys, pathlib
from jsonschema import Draft202012Validator
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

HERE = pathlib.Path(__file__).parent
SCHEMAS, EXAMPLES = HERE / "schemas", HERE / "examples"


def registry() -> Registry:
    """Every local schema, addressable by bare filename and by $id."""
    reg = Registry()
    for path in sorted(SCHEMAS.glob("*.schema.json")):
        doc = json.loads(path.read_text())
        res = Resource.from_contents(doc, default_specification=DRAFT202012)
        reg = reg.with_resource(path.name, res)
        if "$id" in doc:
            reg = reg.with_resource(doc["$id"], res)
    return reg


def main() -> int:
    envelope = json.loads((SCHEMAS / "envelope.schema.json").read_text())
    validator = Draft202012Validator(envelope, registry=registry())

    examples = sorted(EXAMPLES.glob("*.json"))
    if not examples:
        print("no examples found", file=sys.stderr)
        return 1

    failures = 0
    for path in examples:
        doc = json.loads(path.read_text())
        errors = sorted(validator.iter_errors(doc), key=lambda e: list(e.path))
        expect_fail = path.name.startswith("invalid-")

        if errors and not expect_fail:
            failures += 1
            print(f"FAIL  {path.name}")
            for e in errors[:4]:
                loc = "/".join(str(p) for p in e.path) or "(root)"
                print(f"        {loc}: {e.message[:160]}")
        elif not errors and expect_fail:
            failures += 1
            print(f"FAIL  {path.name}  (expected rejection, was accepted)")
        else:
            verb = "rejected" if expect_fail else "ok"
            print(f"  ok  {path.name:44s} {doc.get('type','?'):22s} {verb}")

    print(f"\n{len(examples) - failures}/{len(examples)} passed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
