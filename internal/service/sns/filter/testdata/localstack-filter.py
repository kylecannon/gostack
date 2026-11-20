"""Writes localstack-filter.json: what LocalStack's own filter.py answers for
the policies and messages below, so the Go package is checked against the code
it follows and not against a reading of it.

filter.py imports nothing from LocalStack but InvalidParameterException, which
is stubbed here, so any Python 3 runs it; the committed file came from 3.14.4.
Run it with the LocalStack checkout at 8b9a79f:
  python3 -I localstack-filter.py \
    /Volumes/Development/localstack/localstack-core/localstack/services/sns/filter.py \
    > localstack-filter.json

Each validate case is [policy, scope]; each match case adds a message: an
object of attributes ({"DataType", "StringValue"}) for MessageAttributes, or
the body text for MessageBody. The flow is LocalStack's provider and
publisher: json.loads, no filter when the result is falsy, the validator, then
the scope's check. An exception other than InvalidParameterException is
recorded as a crash, which the Go side answers with a decision of its own.
"""

import importlib.util
import json
import sys
import types


class InvalidParameterException(Exception):
    pass


def load_filter(path):
    api = types.ModuleType("localstack.aws.api.sns")
    api.InvalidParameterException = InvalidParameterException
    for name in ("localstack", "localstack.aws", "localstack.aws.api"):
        sys.modules[name] = types.ModuleType(name)
    sys.modules["localstack.aws.api.sns"] = api
    spec = importlib.util.spec_from_file_location("lsfilter", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


A = "MessageAttributes"
B = "MessageBody"
d = json.dumps

VALIDATE = [
    # shapes
    ["", A], ["{}", A], ["null", A], ["[]", A], ["0", A], ["false", A], ['""', A],
    ["{invalidjson}", A], ["invalid-json", A], ['{"a": ["x"]} trailing', A],
    ["5", A], ["[1]", A], ['"abc"', A], ["true", A],
    [d({"key_a": "value_one"}), A], [d({"key": [["value"]]}), A], [d({"key": []}), A],
    [d({"key": {"suffix": "value"}}), A], [d({"key": {"not-an-operator": "value"}}), A],
    [d({"key": {"suffix": "value", "prefix": "value"}}), A],
    [d({"object": {"key": [{"prefix": "auto-"}]}}), A], [d({"object": {"key": [{"prefix": "auto-"}]}}), B],
    [d({"a": {"b": []}}), A], [d({"a": {"b": "x"}}), A], [d({"a": {}}), A], [d({"a": {}}), B],
    [d({"a": None}), A], [d({"a": 5}), B], [d({"a": True}), B],
    # limits
    [d({f"key{i}": [f"v{i}"] for i in range(1, 6)}), A],
    [d({f"key{i}": [f"v{i}"] for i in range(1, 7)}), A],
    [d({f"key{i}": [f"v{i}"] for i in range(1, 7)}), B],
    [d({"key": [f"value{i}" for i in range(150)]}), A],
    [d({"key": [f"value{i}" for i in range(151)]}), A],
    [d({"key1": [f"value{i}" for i in range(100)], "key2": [f"value{i}" for i in range(51)]}), A],
    [d({"key_a": {"key_b": {"key_c": ["1", "2", "3", "4"]}}, "key_d": {"key_e": ["1", "2", "3"]}}), B],
    [d({"key_a": {"key_b": {"key_c": ["1", "2", "3", "4"]}}, "key_d": {"key_e": ["1", "2", "3"]}, "key_f": ["1", "2", "3"]}), B],
    [d({"a": {"b": {"c": ["x"] * 6}}, "d": {"e": ["y"] * 5}}), B],
    [d({"a": ["x"] * 2000, "b": ["y"] * 2000, "c": ["z"] * 2000, "d": ["w"] * 2000, "e": ["v"] * 2000}), B],
    # $or
    [d({"source": ["a", "b", "c", "d"], "$or": [{"metricName": ["1", "2", "3", "4", "5", "6"]}, {"metricType": ["1", "2", "3", "4"], "$or": [{"metricId": [1, 2, 3, 4]}, {"spaceId": [1, 2, 3, 4]}]}]}), A],
    [d({"source": ["a", "b", "c"], "$or": [{"metricName": ["1", "2", "3", "4", "5", "6"]}, {"metricType": ["1", "2", "3", "4"], "$or": [{"metricId": [1, 2, 3, 4]}, {"spaceId": [1, 2, 3, 4, 5, 6, 7]}]}]}), A],
    [d({"$or": [{"metricName": ["a", "b", "c"]}, {"namespace": ["d", "e"]}], "detail": {"scope": ["s", "t"], "$or": [{"source": ["x"]}, {"type": ["p", "q", "r"]}]}}), B],
    [d({"$or": [{"metricName": ["a", "b"]}, {"namespace": ["d", "e"]}], "detail": {"scope": ["s"], "$or": [{"source": ["x"]}, {"type": ["p"]}]}}), B],
    [d({"$or": []}), A], [d({"$or": [{"a": ["x"]}]}), A], [d({"$or": {"a": ["x"]}}), B], [d({"$or": {"a": ["x"]}}), A],
    [d({"$or": [{}, {}]}), A], [d({"a": {"$or": [{"b": ["x"]}, {"c": ["y"]}]}}), A],
    [d({"$or": ["a", "b"]}), A], [d({"$or": [{"a": ["x"]}, ["b"]]}), B],
    # rules
    [d({"key": [{"wrong-operator": True}]}), A], [d({"key": [{"suffix": "value", "prefix": "value2"}]}), A],
    [d({"key": [{}]}), A], [d({"key": [None, "s", 1, 1.5, True, False]}), A],
    [d({"key": [{"suffix": 100}]}), A], [d({"key": [{"suffix": None}]}), A], [d({"key": [{"suffix": []}]}), A],
    [d({"key": [{"suffix": ["test", "test2"]}]}), A], [d({"key": [{"prefix": 5}]}), A],
    [d({"key": [{"equals-ignore-case": False}]}), A], [d({"key": [{"equals-ignore-case": "X"}]}), A],
    [d({"key": [{"exists": None}]}), A], [d({"key": [{"exists": "no"}]}), A], [d({"key": [{"exists": 1}]}), A],
    [d({"key": [{"exists": False}]}), A],
    [d({"key": [{"anything-but": {"wrong-operator": None}}]}), A],
    [d({"key": [{"anything-but": {"suffix": "test"}}]}), A],
    [d({"key": [{"anything-but": {"exists": False}}]}), A],
    [d({"key": [{"anything-but": {"prefix": False}}]}), A],
    [d({"key": [{"anything-but": {"prefix": "test-"}}]}), A],
    [d({"key": [{"anything-but": {"prefix": "a", "x": 1}}]}), A],
    [d({"key": [{"anything-but": {}}]}), A],
    [d({"key": [{"anything-but": ["test", "test2"]}]}), A],
    [d({"key": [{"anything-but": "test"}]}), A], [d({"key": [{"anything-but": 5}]}), A],
    [d({"key": [{"anything-but": [["x"]]}]}), A], [d({"key": [{"anything-but": [{"prefix": "a"}]}]}), A],
    [d({"key": [{"anything-but": [{"bogus": "a"}]}]}), A],
    # numeric
    [d({"key": [{"numeric": []}]}), A], [d({"key": [{"numeric": ["operator"]}]}), A],
    [d({"key": [{"numeric": [1, "<="]}]}), A], [d({"key": [{"numeric": ["=", "000"]}]}), A],
    [d({"key": [{"numeric": [">="]}]}), A], [d({"key": [{"numeric": ["<", 100, ">", 10]}]}), A],
    [d({"key": [{"numeric": [">=", 1, ">", 2]}]}), A], [d({"key": [{"numeric": [">", 3, "<", 1]}]}), A],
    [d({"key": [{"numeric": [">", 3, "<", 3]}]}), A],
    [d({"key": [{"numeric": [">", 20, "<="]}]}), A], [d({"key": [{"numeric": [">", 20, 30]}]}), A],
    [d({"key": [{"numeric": [">", 20, "test"]}]}), A], [d({"key": [{"numeric": [">", "20", "<", "30"]}]}), A],
    [d({"key": [{"numeric": [">", 20, "<", "30"]}]}), A], [d({"key": [{"numeric": [">", 20, "<", 30, "<", 50]}]}), A],
    [d({"key": [{"numeric": [">", 0, "<=", 100]}]}), A], [d({"key": [{"numeric": ["=", 1.5]}]}), A],
    [d({"key": [{"numeric": [">", True]}]}), A], [d({"key": [{"numeric": [None]}]}), A],
    [d({"key": [{"numeric": [1.5]}]}), A], [d({"key": [{"numeric": [1e20]}]}), A], [d({"key": [{"numeric": [1e-7]}]}), A],
    [d({"key": [{"numeric": [True]}]}), A], [d({"key": [{"numeric": [[1, "a"]]}]}), A],
    [d({"key": [{"numeric": [{"a": 1}]}]}), A], [d({"key": [{"numeric": [">", 1, None]}]}), A],
    [d({"key": [{"numeric": [">", 1, ["<"]]}]}), A],
    [d({"key": [{"numeric": 0}]}), A], [d({"key": [{"numeric": None}]}), A], [d({"key": [{"numeric": ""}]}), A],
    [d({"key": [{"numeric": 5}]}), A], [d({"key": [{"numeric": "abc"}]}), A], [d({"key": [{"numeric": {"a": 1}}]}), A],
    # cidr
    [d({"key": [{"cidr": ["bad-filter"]}]}), A], [d({"key": [{"cidr": "bad-filter"}]}), A],
    [d({"key": [{"cidr": "bad-filter/64"}]}), A], [d({"key": [{"cidr": "bad-/64filter"}]}), A],
    [d({"key": [{"cidr": "xx.11.xx/8"}]}), A], [d({"key": [{"cidr": "xxxx:db8:1234:1a00::/64"}]}), A],
    [d({"key": [{"cidr": "10.0.0.0/24"}]}), A], [d({"key": [{"cidr": "10.0.0.1/24"}]}), A],
    [d({"key": [{"cidr": "10.0.0.0/08"}]}), A], [d({"key": [{"cidr": "10.0.0.0/+8"}]}), A],
    [d({"key": [{"cidr": "10.0.0.0/ 8"}]}), A], [d({"key": [{"cidr": "10.0.0.0/33"}]}), A],
    [d({"key": [{"cidr": "10.0.0.0/"}]}), A], [d({"key": [{"cidr": "/8"}]}), A], [d({"key": [{"cidr": ""}]}), A],
    [d({"key": [{"cidr": "10.0.0.0/8/8"}]}), A], [d({"key": [{"cidr": "010.0.0.0/8"}]}), A],
    [d({"key": [{"cidr": "2001:db8:1234:1a00::/64"}]}), A], [d({"key": [{"cidr": "2001:db8::1/64"}]}), A],
    [d({"key": [{"cidr": "::/0"}]}), A], [d({"key": [{"cidr": "::ffff:10.0.0.0/120"}]}), A],
    [d({"key": [{"cidr": "2001:db8::/129"}]}), A], [d({"key": [{"cidr": "0.0.0.0/0"}]}), A],
    [d({"key": [{"cidr": "10.0.0.0/2_4"}]}), A], [d({"key": [{"cidr": 5}]}), A],
    # order: the first refusal wins
    [d({"a": [], "b": "x"}), A], [d({"a": "x", "b": []}), A], [d({"a": [{"bogus": 1}], "b": []}), A],
    [d({"a": [{"bogus": 1}], "b": [f"v{i}" for i in range(151)]}), A],
    [d({"a": {"b": ["x"]}, "c": "bad"}), A], [d({"a": {"b": "bad"}}), A], [d({"a": {"b": []}}), B],
    [d({"a": [{"prefix": 1}, {"suffix": 2}]}), A],
]

ATTR_POLICIES = [
    {"attr1": [{"numeric": [">", 0, "<=", 100]}]},
    {"attr1": [None, {"anything-but": "whatever"}]},
    {"store": [{"exists": True}]},
    {"store": [{"exists": False}]},
    {"store": ["value1"]},
    {"store": [""]},
    {"store": [None]},
    {"price": [100]},
    {"price": [{"anything-but": [100, 500]}]},
    {"price": [{"numeric": ["=", 301.5]}]},
    {"k": [{"prefix": "ab"}]},
    {"k": [{"prefix": ""}]},
    {"k": [{"suffix": "yz"}]},
    {"k": [{"equals-ignore-case": "HeLLo"}]},
    {"k": [{"anything-but": {"prefix": "ab"}}]},
    {"k": [{"anything-but": ["a", "b"]}]},
    {"k": [{"anything-but": ""}]},
    {"k": [{"anything-but": []}]},
    {"k": [{"cidr": "10.0.0.0/24"}]},
    {"k": [{"numeric": [">=", 10]}]},
    {"k": ["a", "b"], "m": [{"exists": True}]},
    {"$or": [{"k": ["a"]}, {"m": ["b"]}]},
    {"$or": [{"k": ["a"]}]},
    {"k": ["a"], "$or": [{"m": ["b"]}, {"n": [{"exists": False}]}]},
]

ATTR_MESSAGES = [
    {},
    {"attr1": {"DataType": "Number", "StringValue": "99"}},
    {"attr1": {"DataType": "Number", "StringValue": "111"}},
    {"attr1": {"DataType": "String", "StringValue": "whatever"}},
    {"attr1": {"DataType": "String", "StringValue": "other"}},
    {"store": {"DataType": "Number", "StringValue": "99"}, "def": {"DataType": "Number", "StringValue": "99"}},
    {"def": {"DataType": "Number", "StringValue": "99"}},
    {"store": {"DataType": "String", "StringValue": "value1"}},
    {"store": {"DataType": "String", "StringValue": ""}},
    {"store": {"DataType": "Binary"}},
    {"store": {"DataType": "String.Array", "StringValue": d(["value1", "value2"])}},
    {"store": {"DataType": "String.Array", "StringValue": d(["value2", "value3"])}},
    {"store": {"DataType": "String.Array", "StringValue": "[]"}},
    {"store": {"DataType": "String.Array", "StringValue": "[null, \"\"]"}},
    {"store": {"DataType": "String.Array", "StringValue": "not json"}},
    {"store": {"DataType": "String.Array", "StringValue": "\"value1\""}},
    {"store": {"DataType": "String.Array", "StringValue": "{\"value1\": 1}"}},
    {"store": {"DataType": "String.Array", "StringValue": "5"}},
    {"store": {"DataType": "String.Array", "StringValue": ""}},
    {"price": {"DataType": "Number", "StringValue": "100"}},
    {"price": {"DataType": "Number", "StringValue": "101"}},
    {"price": {"DataType": "Number", "StringValue": "301.5"}},
    {"price": {"DataType": "Number", "StringValue": "3.015e2"}},
    {"price": {"DataType": "String", "StringValue": "100"}},
    {"price": {"DataType": "String.Array", "StringValue": "[100, 50]"}},
    {"price": {"DataType": "String.Array", "StringValue": "[100, 500]"}},
    {"k": {"DataType": "String", "StringValue": "abcxyz"}},
    {"k": {"DataType": "String", "StringValue": "hello"}},
    {"k": {"DataType": "String", "StringValue": "a"}},
    {"k": {"DataType": "String", "StringValue": "b"}},
    {"k": {"DataType": "String", "StringValue": "10.0.0.255"}},
    {"k": {"DataType": "String", "StringValue": "10.0.1.0"}},
    {"k": {"DataType": "String", "StringValue": "1_0"}},
    {"k": {"DataType": "String", "StringValue": " 12 "}},
    {"k": {"DataType": "String", "StringValue": "nan"}},
    {"k": {"DataType": "String", "StringValue": "inf"}},
    {"k": {"DataType": "String.Array", "StringValue": "[\"abc\", 12, \"10.0.0.1\"]"}},
    {"k": {"DataType": "String.Array", "StringValue": "[5, true, {\"a\": 1}, [1]]"}},
    {"k": {"DataType": "String", "StringValue": "a"}, "m": {"DataType": "String", "StringValue": "b"}},
    {"m": {"DataType": "String", "StringValue": "b"}},
    {"n": {"DataType": "String", "StringValue": "x"}, "k": {"DataType": "String", "StringValue": "a"}},
]

BODY_POLICIES = [
    {"object": {"key": [{"prefix": "auto-"}, "hardcodedvalue"], "nested_key": [{"exists": False}]}, "test": [{"exists": False}]},
    {"object.nested": ["string.value"]},
    {"object.nested.test": ["string.value"]},
    {"headers": {"route-to": ["queue1"]}},
    {"Records": {"s3": {"object": {"key": [{"prefix": "auto-"}]}}, "eventName": [{"prefix": "ObjectCreated:"}]}},
    {"$or": [{"metricName": ["CPUUtilization", "ReadLatency"]}, {"namespace": ["AWS/EC2", "AWS/ES"]}], "detail": {"scope": ["Service"], "$or": [{"source": ["aws.cloudwatch"]}, {"type": ["CloudWatch Alarm State Change"]}]}},
    {"detail": {"eventVersion": [""]}},
    {"detail": {"$or": [{"sourceIPAddress": [{"cidr": "10.0.0.0/24"}]}, {"sourceIPAddressV6": [{"cidr": "2001:db8:1234:1a00::/64"}]}]}},
    {"a": {"b": [{"exists": False}]}},
    {"a": {"b": [{"exists": True}]}},
    {"a": [1], "b": [True]},
    {"a": [None]},
    {"a": [{"anything-but": [1, "x"]}]},
    {"a": [{"anything-but": 0}]},
    {"a": [{"numeric": [">", 0]}]},
    {"a": [{"prefix": "x"}]},
    {"a": [{"equals-ignore-case": "X"}]},
    {"a": [{"cidr": "0.0.0.0/24"}]},
    {"a": [{"cidr": "::/64"}]},
    {"": ["x"]},
    {"x": ["v"]},
    {"a": {"b": ["v"]}, "c": ["w"]},
    {"$or": [{"a": ["x"]}]},
]

BODY_MESSAGES = [
    "Regular string message", d("Regular string message"), "[1, 2]", "", "{", "null", "{}",
    d({"object": {"key": "auto-test"}}), d({"object": {"key": "hardcodedvalue"}}),
    d({"object": {"key": "test-auto"}}), d({"object": {"key": "auto-test"}, "test": "just-exists"}),
    d({"object": {"key": "auto-test", "nested_key": "just-exists"}}), d({"object": {"test": "auto-test"}}),
    d({"test": "auto-test"}),
    d({"object": {"nested": "string.value"}}), d({"object.nested": "string.value"}),
    d({"object": {"nested": "test-auto"}}),
    d({"object": {"nested": {"test": "string.value"}}}), d({"object.nested.test": "string.value"}),
    d({"object.nested": {"test": "string.value"}}), d({"object": {"nested.test": "string.value"}}),
    d({"object": {"nested": {"test": "string.notvalue"}}}),
    '{"object": {"nested": "string.value"}, "object.nested": "other"}',
    '{"object.nested": "other", "object": {"nested": "string.value"}}',
    '{"object": {"nested": "x", "nested": "string.value"}}',
    d({"headers": {"route-to": ["queue3"]}}), d({"headers": {"route-to": ["queue1"]}}),
    d({"headers": {"route-to": ["queue1", "queue2"]}}), d({"headers": [{"route-to": "queue1"}, {"route-to": "queue2"}]}),
    d({"Records": [{"eventName": "ObjectCreated:Put", "s3": {"object": [[{"key": "auto-insurance-2314.xml", "size": 17}]]}},
                   {"eventName": "ObjectRemoved:Delete", "s3": {"object": {"key": "home-insurance-2314.xml"}}}]}),
    d({"Records": [{"eventName": "ObjectRemoved:Delete", "s3": {"object": {"key": "auto-insurance-2314.xml"}}}]}),
    d({"Records": [{"eventName": "ObjectCreated:Put", "s3": {"object": {"key": "home-insurance-2314.xml"}}}]}),
    d({"Records": [{"eventName": "ObjectCreated:Put", "s3": {"object": {"key": "auto-insurance-2314.xml"}}}]}),
    d({"metricName": "CPUUtilization", "detail": {"scope": "aws.cloudwatch", "type": "CloudWatch Alarm State Change"}}),
    d({"metricName": "CPUUtilization", "detail": {"scope": "Service", "type": "CPUUtilization"}}),
    d({"metricName": "CPUUtilization", "detail": {"type": "CloudWatch Alarm State Change"}}),
    d({"metricName": "CPUUtilization", "detail": {"scope": "Service"}}),
    d({"metricName": "CPUUtilization", "scope": "Service"}),
    d({"metricName": "AWS/EC2", "detail": {"scope": "Service", "type": "CloudWatch Alarm State Change"}}),
    d({"namespace": "CPUUtilization", "detail": {"scope": "Service", "type": "CloudWatch Alarm State Change"}}),
    d({"metricName": "CPUUtilization", "detail": {"scope": "Service", "source": "aws.cloudwatch"}}),
    d({"metricName": "ReadLatency", "detail": {"scope": "Service", "source": "aws.cloudwatch"}}),
    d({"namespace": "AWS/EC2", "detail": {"scope": "Service", "source": "aws.cloudwatch"}}),
    d({"namespace": "AWS/ES", "detail": {"scope": "Service", "source": "aws.cloudwatch"}}),
    d({"metricName": "CPUUtilization", "detail": {"scope": "Service", "type": "CloudWatch Alarm State Change"}}),
    d({"version": "0", "resources": [], "detail": {"eventVersion": "", "responseElements": None}}),
    d({"detail": {"eventVersion": None}}), d({"detail": {"eventVersion": []}}), d({"detail": {"eventVersion": [""]}}),
    d({"detail": {"sourceIPAddress": "10.0.0.255"}}), d({"detail": {"sourceIPAddress": "10.0.0.256"}}),
    d({"detail": {"sourceIPAddressV6": "2001:0db8:1234:1a00:0000:0000:0000:0000"}}),
    d({"detail": {"sourceIPAddressV6": "2001:0db8:123f:1a01:0000:0000:0000:0000"}}),
    d({"a": [{"b": 1}, {"c": 2}]}), d({"a": [{"b": 1}, {"b": 2}]}), d({"a": {"b": 1}}), d({"a": {}}), d({"a": []}),
    d({"a": {"b": []}}), d({"a": {"b": None}}), d({"a": 1}),
    d({"a": 1, "b": 1}), d({"a": True, "b": 1}), d({"a": 1.0, "b": True}), d({"a": "1", "b": "true"}),
    d({"a": None}), d({"a": [None, 1]}), d({"a": "x"}), d({"a": 0}), d({"a": [1, "x", 2]}),
    d({"a": "x1"}), d({"a": 5}), d({"a": "5"}), d({"a": "-1"}), d({"a": False}), d({"a": True}),
    d({"a": 3}), d({"a": "0.0.0.3"}), d({"a": "::1"}), d({"a": 18446744073709551615}),
    d({"": "x"}), d({"": {"x": "v"}}), d({"": {"": "x"}}), d({"x": {"": "v"}}),
    d({"a": {"b": "v"}, "c": "w"}), d({"a": [{"b": "v"}, {"b": "u"}], "c": ["q", "w"]}), d({"a": {"b": "v"}}),
    d({"$or": "x"}),
]


def run(filt, policy, scope, message=None):
    if not policy:
        fp = None
    else:
        try:
            fp = json.loads(policy)
        except json.JSONDecodeError:
            return {"error": "FilterPolicy: failed to parse JSON."}
    if fp:
        try:
            filt.FilterPolicyValidator(scope=scope, is_subscribe_call=False).validate_filter_policy(fp)
        except InvalidParameterException as e:
            return {"error": str(e).removeprefix("Invalid parameter: ")}
        except Exception as e:  # noqa: BLE001 - a LocalStack crash is the data
            return {"crash": type(e).__name__}
    if message is None:
        return {"ok": bool(fp)}
    if not fp:
        return {"match": True}
    sub = filt.SubscriptionFilter()
    try:
        if scope == A:
            return {"match": bool(sub.check_filter_policy_on_message_attributes(fp, message))}
        return {"match": bool(sub.check_filter_policy_on_message_body(fp, message))}
    except Exception as e:  # noqa: BLE001
        return {"crash": type(e).__name__}


def main():
    filt = load_filter(sys.argv[1])
    out = {"validate": [], "match": []}
    for policy, scope in VALIDATE:
        out["validate"].append({"policy": policy, "scope": scope, **run(filt, policy, scope)})
    for scope, policies, messages in ((A, ATTR_POLICIES, ATTR_MESSAGES), (B, BODY_POLICIES, BODY_MESSAGES)):
        for policy in policies:
            for message in messages:
                out["match"].append(
                    {"policy": d(policy), "scope": scope, "message": message, **run(filt, d(policy), scope, message)}
                )
    json.dump(out, sys.stdout, indent=None, separators=(",", ":"))
    sys.stdout.write("\n")


main()
