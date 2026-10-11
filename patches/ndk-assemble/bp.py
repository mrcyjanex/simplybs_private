"""Minimal Android.bp extractor: srcs, cflags, includes, filegroups, globs."""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from pathlib import Path

_NAME_RE = re.compile(r'name:\s*"([^"]+)"')
_STR_RE = re.compile(r'"((?:\\.|[^"\\])*)"')


def _unescape(s: str) -> str:
    return bytes(s, "utf-8").decode("unicode_escape") if "\\" in s else s


def _strip_comments(text: str) -> str:
    out: list[str] = []
    for line in text.splitlines():
        in_str = False
        esc = False
        for i, ch in enumerate(line):
            if in_str:
                if esc:
                    esc = False
                elif ch == "\\":
                    esc = True
                elif ch == '"':
                    in_str = False
                continue
            if ch == '"':
                in_str = True
                continue
            if ch == "/" and i + 1 < len(line) and line[i + 1] == "/":
                line = line[:i]
                break
        out.append(line)
    return "\n".join(out)


def _extract_bracket(text: str, start: int = 0) -> tuple[str, int]:
    """Return contents of the `[...]` or `{...}` starting at `start`, and end index."""
    opener = text[start]
    closer = {"[": "]", "{": "}"}.get(opener)
    if closer is None:
        raise ValueError(f"expected [ or {{ at {start}")
    depth = 0
    for j in range(start, len(text)):
        if text[j] == opener:
            depth += 1
        elif text[j] == closer:
            depth -= 1
            if depth == 0:
                return text[start + 1 : j], j + 1
    return text[start + 1 :], len(text)


def _enclosing_block(text: str, name: str) -> str | None:
    """Return the `{...}` body of the module whose name field is `name`."""
    for m in _NAME_RE.finditer(text):
        if m.group(1) != name:
            continue
        i = m.start()
        depth = 0
        start = None
        while i >= 0:
            if text[i] == "}":
                depth += 1
            elif text[i] == "{":
                if depth == 0:
                    start = i
                    break
                depth -= 1
            i -= 1
        if start is None:
            continue
        depth = 0
        for j in range(start, len(text)):
            if text[j] == "{":
                depth += 1
            elif text[j] == "}":
                depth -= 1
                if depth == 0:
                    return text[start + 1 : j]
        return None
    return None


def _list_after(block: str, key: str) -> list[str]:
    token = f"{key}:"
    depth = 0
    i = 0
    while i < len(block):
        ch = block[i]
        if ch == "{":
            depth += 1
            i += 1
            continue
        if ch == "}":
            depth -= 1
            i += 1
            continue
        if depth == 0 and block.startswith(token, i):
            rest = block[i + len(token) :].lstrip()
            if rest.startswith("["):
                body, _ = _extract_bracket(rest, 0)
                return [_unescape(s) for s in _STR_RE.findall(body)]
            m = _STR_RE.match(rest)
            return [_unescape(m.group(1))] if m else []
        i += 1
    return []


def _arch_block(block: str, arch: str) -> str:
    m = re.search(rf"\b{re.escape(arch)}\s*:\s*\{{", block)
    if not m:
        return ""
    body, _ = _extract_bracket(block, m.end() - 1)
    return body


@dataclass
class Module:
    name: str
    srcs: list[str] = field(default_factory=list)
    exclude_srcs: list[str] = field(default_factory=list)
    cflags: list[str] = field(default_factory=list)
    local_include_dirs: list[str] = field(default_factory=list)
    include_dirs: list[str] = field(default_factory=list)
    defaults: list[str] = field(default_factory=list)
    whole_static_libs: list[str] = field(default_factory=list)
    out: list[str] = field(default_factory=list)
    filegroups: list[str] = field(default_factory=list)
    arch: dict[str, dict[str, list[str]]] = field(default_factory=dict)
    root: Path | None = None
    generated: bool = False


def parse_module(block: str, name: str, root: Path | None = None) -> Module:
    mod = Module(name=name, root=root)
    raw_srcs = _list_after(block, "srcs")
    mod.srcs = [s for s in raw_srcs if not s.startswith(":")]
    mod.filegroups = [s[1:] for s in raw_srcs if s.startswith(":")]
    mod.exclude_srcs = _list_after(block, "exclude_srcs")
    mod.cflags = _list_after(block, "cflags")
    mod.local_include_dirs = _list_after(block, "local_include_dirs")
    mod.include_dirs = _list_after(block, "include_dirs")
    mod.defaults = _list_after(block, "defaults")
    mod.whole_static_libs = [s for s in _list_after(block, "whole_static_libs") if not s.startswith("//")]
    mod.out = _list_after(block, "out")
    mod.generated = bool(mod.out)
    for arch in ("arm", "arm64", "riscv64", "x86", "x86_64"):
        ab = _arch_block(block, arch)
        if not ab:
            continue
        raw_srcs = _list_after(ab, "srcs")
        mod.arch[arch] = {
            "srcs": [s for s in raw_srcs if not s.startswith(":")],
            "filegroups": [s[1:] for s in raw_srcs if s.startswith(":")],
            "exclude_srcs": _list_after(ab, "exclude_srcs"),
            "cflags": _list_after(ab, "cflags"),
            "local_include_dirs": _list_after(ab, "local_include_dirs"),
        }
    return mod


def parse_blueprint(text: str, root: Path | None = None) -> dict[str, Module]:
    mods: dict[str, Module] = {}
    text = _strip_comments(text)
    for m in _NAME_RE.finditer(text):
        name = m.group(1)
        block = _enclosing_block(text, name)
        if block is None:
            continue
        mods[name] = parse_module(block, name, root)
    return mods


def index_blueprints(bp: Path | str, extra: list[Path] | None = None) -> dict[str, Module]:
    path = Path(bp) if not isinstance(bp, str) or (len(bp) < 512 and Path(bp).is_file()) else None
    if path is not None and path.is_file():
        mods: dict[str, Module] = {}
        seen: set[Path] = set()
        files = [path]
        files.extend(path.parent.rglob("Android.bp"))
        if path.parent.name == "libc":
            files.extend(path.parent.parent.glob("*/Android.bp"))
        if extra:
            files.extend(extra)
        for f in files:
            f = f.resolve()
            if f in seen or not f.is_file():
                continue
            seen.add(f)
            mods.update(parse_blueprint(f.read_text(), f.parent))
        return mods
    return parse_blueprint(str(bp), None)


def _expand_one(mods: dict[str, Module], name: str, arch: str | None, seen: set[str]) -> list[tuple[Path | None, str]]:
    if name in seen:
        return []
    seen.add(name)
    mod = mods.get(name)
    if mod is None or mod.generated:
        return []
    out: list[tuple[Path | None, str]] = []
    for d in mod.defaults:
        out.extend(_expand_one(mods, d, arch, seen))
    for rel in mod.srcs:
        out.append((mod.root, rel))
    for fg in getattr(mod, "filegroups", []):
        out.extend(_expand_one(mods, fg, arch, seen))
    if arch and arch in mod.arch:
        info = mod.arch[arch]
        for rel in info.get("srcs", []):
            out.append((mod.root, rel))
        for fg in info.get("filegroups", []):
            out.extend(_expand_one(mods, fg, arch, seen))
        excl = set(info.get("exclude_srcs", []))
        if excl:
            out = [p for p in out if p[1] not in excl]
    excl_all = set(mod.exclude_srcs)
    if excl_all:
        out = [p for p in out if p[1] not in excl_all]
    return out


def _glob_rel(root: Path | None, rel: str) -> list[Path]:
    if root is None:
        return []
    if any(ch in rel for ch in "*?["):
        return sorted(p for p in root.glob(rel) if p.is_file())
    path = root / rel
    return [path] if path.is_file() else []


def resolve_module_srcs(mods: dict[str, Module], name: str, arch: str | None = None) -> list[Path]:
    found: list[Path] = []
    seen_path: set[Path] = set()
    for root, rel in _expand_one(mods, name, arch, set()):
        for path in _glob_rel(root, rel):
            if path not in seen_path:
                seen_path.add(path)
                found.append(path)
    return found


def module_cflags(mods: dict[str, Module], name: str, arch: str | None = None) -> list[str]:
    if name not in mods:
        return []
    seen: set[str] = set()
    flags: list[str] = []

    def walk(n: str) -> None:
        if n in seen or n not in mods:
            return
        seen.add(n)
        mod = mods[n]
        for d in mod.defaults:
            walk(d)
        flags.extend(mod.cflags)
        if arch and arch in mod.arch:
            flags.extend(mod.arch[arch].get("cflags", []))

    walk(name)
    return flags


def module_includes(mods: dict[str, Module], name: str, arch: str | None = None) -> list[Path]:
    if name not in mods:
        return []
    seen: set[str] = set()
    dirs: list[Path] = []

    def walk(n: str) -> None:
        if n in seen or n not in mods:
            return
        seen.add(n)
        mod = mods[n]
        for d in mod.defaults:
            walk(d)
        if mod.root is not None:
            for rel in mod.local_include_dirs:
                dirs.append(mod.root / rel)
        if arch and arch in mod.arch and mod.root is not None:
            for rel in mod.arch[arch].get("local_include_dirs", []):
                dirs.append(mod.root / rel)

    walk(name)
    return dirs
