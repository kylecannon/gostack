"""Writes python-parse.json: what Python 3.13 makes of the scalars LocalStack's
request parser reads with int(), float() and base64.b64decode().

Run it with the interpreter in gostack-parity-localstack:8b9a79f, for example
  docker cp python-parse.py <container>:/tmp/ && docker exec <container> \
    /opt/code/localstack/.venv/bin/python /tmp/python-parse.py > python-parse.json
"""

import base64
import itertools
import json
import math

I64_MAX, I64_MIN = 2**63 - 1, -(2**63)

INTS = [
    "", " ", "0", "5", " 5", "5 ", " 5 ", "\t5\n", "\x1c5", "\x855", "\xa05", "　5", "+5", "-5", "+", "-", "--5", "+-5",
    "1_0", "1__0", "_1", "1_", "1_0_0", "+1_0", "- 5", "5 5", "007", "-0", "+0", "5.0", "5.", ".5", "1e1", "0x10", "0b1",
    "٥", "٥٦", "۵", "５", "\U0001d7d7", "\U0001d7ec", "5٥", "²", "⅕", "五",
    "2147483647", "2147483648", "-2147483648", "-2147483649", "3000000000", "9223372036854775807",
    "9223372036854775808", "-9223372036854775808", "-9223372036854775809", "99999999999999999999",
    "1" * 4300, "1" * 4301, "1_" * 5 + "1", "abc", "5a", "a5", "NaN", "inf", "true",
]

FLOATS = [
    "", " ", "0", "5", " 5 ", "+5", "-5", "5.", ".5", "5.0", "5.5e3", "5e-3", "5E+3", "e5", "5e", "5e+", ".", "+.", "-.5",
    "1_0", "1_0.5_5", "1__0", "_1", "1_", "1_e5", "1e1_0", "inf", "INF", "-inf", "+Infinity", "infinity", "Infinit", "--inf",
    "nan", "NaN", "-nan", "+NaN", "nan1", "0x10", "0x1p-2", "1e400", "-1e400", "1e-400", "٥", "５.５",
    " 5\t", "5 5", "1,5", "abc", "true",
]

B64_ALPHABET = ["A", "B", "a", "Z", "0", "+", "/", "=", "-", "_", " ", "\n", "$", "ä"]
B64 = ["", "=", "==", "===", "===="]
B64 += ["aGVsbG8=", "aGVsbG8", "aGVsbA==", "aGVsbA=", "aGVsbA", "aGVsb", "aGVsbG8==", "aGVsbG8=aGVsbG8=", "aGVsbA==aGVsbA==",
        "aGVs\nbG8=", "aGVs bG8=", "-_-_", "aGV$sbG8=", "aGVsäbG8=", "QQ==\n", "QQ=\n=", "Q=Q=", "QQ=A", "====QQ=="]
for n in range(1, 7):
    B64 += ["".join(t) for t in itertools.product(B64_ALPHABET[:9], repeat=n)] if n <= 4 else []


def clamp(v):
    return max(I64_MIN, min(I64_MAX, v))


def py_int(s):
    try:
        return {"in": s, "ok": True, "v": clamp(int(s))}
    except ValueError:
        return {"in": s, "ok": False}


def py_float(s):
    try:
        f = float(s)
    except ValueError:
        return {"in": s, "ok": False}
    if math.isnan(f):
        return {"in": s, "ok": True, "nan": True}
    if math.isinf(f):
        return {"in": s, "ok": True, "inf": 1 if f > 0 else -1}
    return {"in": s, "ok": True, "v": f}


def py_b64(s):
    try:
        out = base64.b64decode(s.encode("ascii"))
    except (ValueError, UnicodeEncodeError):
        return {"in": s, "ok": False}
    return {"in": s, "ok": True, "hex": out.hex()}


print(json.dumps({
    "int": [py_int(s) for s in INTS],
    "float": [py_float(s) for s in FLOATS],
    "base64": [py_b64(s) for s in dict.fromkeys(B64)],
}, ensure_ascii=True, separators=(",", ":")))
