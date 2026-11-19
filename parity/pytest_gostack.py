"""pytest plugin for the gostack parity runner, loaded with ``-p pytest_gostack``.

It does two things to LocalStack's own test suite, which is otherwise run
unchanged against a container:

1. Config bridge (only when ``GOSTACK_PARITY_BRIDGE=1``).
   About 130 LocalStack tests ``monkeypatch.setattr(localstack.config, ...)``.
   That changes the runner's copy of the config, which the target container
   never sees. The plugin also POSTs each change to the target's
   ``/_localstack/config`` (``{"variable": name, "value": value}``, LocalStack's
   own format, enabled on the target with ``ENABLE_CONFIG_UPDATES=1``) and POSTs
   the previous value back when the monkeypatch is undone. A failed POST raises,
   so the test fails instead of silently running against the wrong config.

2. Exclusions (``GOSTACK_EXCLUSIONS``, a YAML file of ``pattern: reason``).
   Matching tests are skipped with the reason ``excluded: <reason>``. Tests
   carrying the ``requires_in_process`` marker are excluded too, unless the
   bridge is on and ``GOSTACK_BRIDGEABLE`` (a YAML file of ``pattern: note``)
   lists them: those tests only patch ``localstack.config``, which the bridge
   forwards, so they run. ``GOSTACK_EXCLUDED_OUT`` names the JSON file that
   records every excluded nodeid and its reason, for ``parity/ledger.py``.

   A pattern is a nodeid or a nodeid with ``*`` wildcards. A pattern without a
   wildcard matches that nodeid and all of its parametrizations
   (``nodeid[...]``).

3. ``GOSTACK_PARITY_TARGET`` (``gostack`` or ``localstack``) names the container
   under test. LocalStack's ``/_localstack/config`` stores the JSON value it is
   given as it is, so ``LOCALSTACK_HOST`` posted as text would replace the
   ``HostAndPort`` object that ``localstack_host()`` reads with a ``str`` and
   break every queue URL after it. The bridge does not post that one variable
   to a ``localstack`` target. gostack parses the text.
"""

from __future__ import annotations

import importlib
import json
import os
import re
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

import pytest
import yaml
from _pytest.monkeypatch import MonkeyPatch, notset

CONFIG_URL = os.environ.get("GOSTACK_PARITY_CONFIG_URL", "http://localhost:4566/_localstack/config")
REQUIRES_IN_PROCESS = "requires_in_process"

# Handed from the exclusion hook to the report hook that runs after deselection.
_pending: dict[str, Any] = {}
# Variables whose value LocalStack's config endpoint cannot hold as the object
# the server reads (see the module docstring, item 3).
NOT_POSTED_TO_LOCALSTACK = frozenset({"LOCALSTACK_HOST"})

# --- config bridge ---------------------------------------------------------


def _bridge_enabled() -> bool:
    return os.environ.get("GOSTACK_PARITY_BRIDGE") == "1"


def _encode_value(value: Any) -> Any:
    """JSON form of a config value that is not JSON: a HostAndPort is ``host:port``."""
    host_and_port = getattr(value, "host_and_port", None)
    if callable(host_and_port):
        return host_and_port()
    raise TypeError(f"config bridge: cannot send {type(value).__name__} {value!r}")


def _post_config(variable: str, value: Any) -> None:
    """POST one config variable to the target. Raises on any failure."""
    if variable in NOT_POSTED_TO_LOCALSTACK and os.environ.get("GOSTACK_PARITY_TARGET") == "localstack":
        return
    body = json.dumps({"variable": variable, "value": value}, default=_encode_value).encode()
    request = urllib.request.Request(
        CONFIG_URL, data=body, headers={"Content-Type": "application/json"}, method="POST"
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            response.read()
    except (urllib.error.URLError, OSError) as err:
        raise RuntimeError(f"config bridge: POST {CONFIG_URL} {variable}={value!r} failed: {err}") from err


def _config_module() -> Any:
    import localstack.config

    return localstack.config


def _resolve_setattr_target(target: Any, name: Any, value: Any) -> tuple[Any, str, Any] | None:
    """Return (object, attribute, value) for either setattr form, or None."""
    if value is notset:
        if not isinstance(target, str):
            return None
        module_path, _, attribute = target.rpartition(".")
        if not module_path:
            return None
        try:
            return importlib.import_module(module_path), attribute, name
        except ImportError:
            return None
    if not isinstance(name, str):
        return None
    return target, name, value


def _coerce_env(current: Any, text: str) -> Any:
    if isinstance(current, bool):
        return text.strip().lower() in ("1", "true")
    if isinstance(current, (int, float)):
        return float(text)
    return text


def _install_bridge() -> None:
    original_setattr = MonkeyPatch.setattr
    original_setenv = MonkeyPatch.setenv
    original_undo = MonkeyPatch.undo

    def setattr_(self: MonkeyPatch, target: Any, name: Any = notset, value: Any = notset, raising: bool = True) -> None:
        resolved = _resolve_setattr_target(target, name, value)
        config = _config_module()
        bridged = resolved is not None and resolved[0] is config
        previous = getattr(config, resolved[1], notset) if bridged else notset
        original_setattr(self, target, name, value, raising)
        if bridged:
            _, attribute, new_value = resolved
            self.__dict__.setdefault("_gostack_bridged", []).append((attribute, previous))
            _post_config(attribute, new_value)

    def setenv_(self: MonkeyPatch, name: str, value: Any, prepend: str | None = None) -> None:
        config = _config_module()
        bridged = name in getattr(config, "CONFIG_ENV_VARS", ()) and hasattr(config, name)
        previous = getattr(config, name, notset) if bridged else notset
        original_setenv(self, name, value, prepend)
        if bridged:
            self.__dict__.setdefault("_gostack_bridged", []).append((name, previous))
            _post_config(name, _coerce_env(previous, str(value)))

    def undo_(self: MonkeyPatch) -> None:
        bridged = self.__dict__.pop("_gostack_bridged", [])
        original_undo(self)
        failures = []
        for attribute, previous in reversed(bridged):
            if previous is notset or previous is None:
                # LocalStack ignores a null value, so there is nothing to restore.
                continue
            try:
                _post_config(attribute, previous)
            except RuntimeError as err:
                failures.append(str(err))
        if failures:
            raise RuntimeError("; ".join(failures))

    MonkeyPatch.setattr = setattr_  # type: ignore[method-assign,assignment]
    MonkeyPatch.setenv = setenv_  # type: ignore[method-assign]
    MonkeyPatch.undo = undo_  # type: ignore[method-assign]


# --- exclusions ------------------------------------------------------------


class _Pattern:
    def __init__(self, text: str, reason: str) -> None:
        self.text = text
        self.reason = reason
        self.matched = False
        if "*" in text:
            self._regex = re.compile("^" + ".*".join(re.escape(part) for part in text.split("*")) + "$")
        else:
            self._regex = None

    def matches(self, nodeid: str) -> bool:
        if self._regex is not None:
            return self._regex.match(nodeid) is not None
        return nodeid == self.text or nodeid.startswith(self.text + "[")

    @property
    def path(self) -> str:
        return self.text.split("::", 1)[0]


def _load_patterns(path: str) -> list[_Pattern]:
    with open(path, encoding="utf-8") as handle:
        loaded = yaml.safe_load(handle) or {}
    if not isinstance(loaded, dict):
        raise pytest.UsageError(f"{path}: expected a mapping of nodeid pattern to reason")
    patterns = []
    for text, reason in loaded.items():
        if not isinstance(text, str) or not isinstance(reason, str) or not reason.strip():
            raise pytest.UsageError(f"{path}: {text!r} needs a non-empty string reason")
        patterns.append(_Pattern(text, reason.strip()))
    return patterns


@pytest.hookimpl(tryfirst=True)
def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    exclusions_file = os.environ.get("GOSTACK_EXCLUSIONS")
    patterns = _load_patterns(exclusions_file) if exclusions_file else []
    bridgeable_file = os.environ.get("GOSTACK_BRIDGEABLE")
    bridgeable = _load_patterns(bridgeable_file) if bridgeable_file and _bridge_enabled() else []
    excluded: dict[str, str] = {}
    for item in items:
        reason = None
        for pattern in patterns:
            if pattern.matches(item.nodeid):
                pattern.matched = True
                reason = reason or pattern.reason
        listed = False
        for pattern in bridgeable:
            if pattern.matches(item.nodeid):
                pattern.matched = True
                listed = True
        if reason is None and not listed and item.get_closest_marker(REQUIRES_IN_PROCESS) is not None:
            reason = REQUIRES_IN_PROCESS
        if reason is not None:
            excluded[item.nodeid] = reason
            item.add_marker(pytest.mark.skip(reason=f"excluded: {reason}"))

    _pending["excluded"] = excluded
    _pending["unmatched"] = {
        "patterns": [*patterns, *bridgeable],
        "paths": {item.nodeid.split("::", 1)[0] for item in items},
    }


def pytest_collection_finish(session: pytest.Session) -> None:
    """Record the excluded nodeids once -k and friends have deselected.

    pytest's own deselection runs after the hook above. A nodeid that was
    collected but deselected is not run, so it must not reach the ledger.
    """
    out = os.environ.get("GOSTACK_EXCLUDED_OUT")
    if not out or "excluded" not in _pending:
        return
    items = session.items
    kept = {item.nodeid for item in items}
    excluded = {nodeid: reason for nodeid, reason in _pending["excluded"].items() if nodeid in kept}
    seen = _pending["unmatched"]
    unmatched = [p.text for p in seen["patterns"] if not p.matched and p.path in seen["paths"]]
    Path(out).write_text(
        json.dumps({"collected": len(items), "excluded": excluded, "unmatched": unmatched}, indent=1, sort_keys=True)
        + "\n",
        encoding="utf-8",
    )
    for text in unmatched:
        print(f"pytest_gostack: exclusion matches nothing: {text}")


def pytest_configure(config: pytest.Config) -> None:
    if _bridge_enabled():
        _install_bridge()


@pytest.fixture(autouse=True)
def _gostack_snapshot_headers(request: pytest.FixtureRequest) -> None:
    """Keep ``x-gostack`` out of snapshots, as LocalStack's conftest does for ``x-localstack``.

    Every gostack response carries both headers. A test that snapshots
    ``ResponseMetadata.HTTPHeaders`` would otherwise differ from its recorded
    AWS snapshot by the one header. Only tests that already use the ``snapshot``
    fixture get the transformer.
    """
    if "snapshot" not in request.fixturenames:
        return
    snapshot = request.getfixturevalue("snapshot")
    snapshot.add_transformer(snapshot.transform.remove_key("x-gostack"))
