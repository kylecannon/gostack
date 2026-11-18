"""Writes werkzeug-form.json, the cases form_test.go checks ParseValues against.

Run it inside the LocalStack image the parity harness builds (werkzeug 3.1.6,
Python 3.13), so the expected pairs are what LocalStack itself reads:

    docker run --rm -v "$PWD/internal/protocol/query/testdata:/t" \
        --entrypoint /opt/code/localstack/.venv/bin/python \
        gostack-parity-localstack:8b9a79f /t/werkzeug-form.py /t/werkzeug-form.json

It calls the same parse_qsl call werkzeug makes for request.args and for a
urlencoded request.form, after importing werkzeug.urls, which registers the
"werkzeug.url_quote" error handler. The random cases use a fixed seed.
"""
import json
import random
import sys
from urllib.parse import parse_qsl

import werkzeug.urls  # noqa: F401  (registers the error handler)

HANDWRITTEN = [
    "", "&", "&&", "a", "a=", "=a", "=", "a=b", "a=b&c=d", "a=b&a=c", "a=b;c=d", "a;b=c",
    "a=1;b=2", "a=%zz", "a=%", "a=%a", "a=%A", "a=%%41", "a=%4", "a=%41", "a=%4g",
    "a=+", "a=%2B", "a=b+c%20d", "+a=b", "a%3Db=c", "a%26b=c", "a=b=c",
    "a=%FF", "a=%ff", "a=%Ff", "a=a%FFb", "a=%FF%FE", "a=%C3", "a=%C3%A9", "a=%c3%a9",
    "a=%E2%82", "a=%E2%82%AC", "a=%E2%82A", "a=%E2%82%ACb", "a=%ED%A0%80", "a=%C0%80",
    "a=%F4%90%80%80", "a=%F0%9F%98%80", "a=%F0%9F%98", "a=%80", "a=%C3é", "a=é%C3",
    "a=%C3%A9é", "é=%FF", "a=€%E2%82", "a=%00", "a=%0A", "a=%0d%0a",
    "a=b&&c=d", "a=b&", "&a=b", "a&b&c", "a=b&c", "a=1&b=%zz&c=3", "Action=Send&Body=a%FFb",
    "a=%EF%BF%BD", "a=%EF%BF%BF", "a=%F4%8F%BF%BF", "a=%F4%8F%BF", "a=%E0%80%80", "a=%E0%A0%80",
]

ALPHABET = ["a", "b", "Z", "0", "1", "9", "=", "&", ";", "+", "%", "%", "%", " ", "é", "€",
            "%FF", "%ff", "%C3", "%A9", "%E2", "%82", "%AC", "%F0", "%9F", "%98", "%80", "%7F", "%zz",
            "%4", "%41", "%2B", "%26", "%3D", "%ED", "%A0", "%c0", "%Fe"]


def main(out):
    rng = random.Random(46)
    cases = list(HANDWRITTEN)
    for _ in range(1500):
        cases.append("".join(rng.choice(ALPHABET) for _ in range(rng.randint(1, 14))))
    result = []
    for text in cases:
        pairs = parse_qsl(text, keep_blank_values=True, errors="werkzeug.url_quote")
        result.append({"in": text, "pairs": [list(p) for p in pairs]})
    with open(out, "w", encoding="utf-8") as f:
        json.dump(result, f, ensure_ascii=False, indent=0)
        f.write("\n")


if __name__ == "__main__":
    main(sys.argv[1])
