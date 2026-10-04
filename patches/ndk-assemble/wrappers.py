"""Generate NDK-style Clang wrapper scripts."""

from __future__ import annotations

from pathlib import Path

WRAPPER = """\
#!/bin/bash
bin_dir=`dirname "$0"`
if [ "$1" != "-cc1" ]; then
    exec "$bin_dir/{driver}" --target={target} "$@"
else
    exec "$bin_dir/{driver}" "$@"
fi
"""

COMPAT = """\
#!/bin/sh
exec "$(cd "$(dirname "$0")/{rel}" && pwd)/{name}" "$@"
"""

LD_WRAPPER = """\
#!/bin/sh
exec "$(dirname "$0")/ld.lld" "$@"
"""


def write_executable(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(0o755)


def clang_wrapper(driver: str, target: str) -> str:
    return WRAPPER.format(driver=driver, target=target)


def write_clang_wrappers(bin_dir: Path, clang_triple: str, api_levels: list[int]) -> None:
    for api in api_levels:
        target = f"{clang_triple}{api}"
        write_executable(bin_dir / f"{target}-clang", clang_wrapper("clang", target))
        write_executable(bin_dir / f"{target}-clang++", clang_wrapper("clang++", target))
    if api_levels:
        min_api = min(api_levels)
        write_executable(
            bin_dir / f"{clang_triple}-clang",
            clang_wrapper("clang", f"{clang_triple}{min_api}"),
        )
        write_executable(
            bin_dir / f"{clang_triple}-clang++",
            clang_wrapper("clang++", f"{clang_triple}{min_api}"),
        )


def write_ld_wrapper(bin_dir: Path, lib_triple: str) -> None:
    write_executable(bin_dir / f"{lib_triple}-ld", LD_WRAPPER)


def write_compat_wrapper(path: Path, toolchain_bin: Path, name: str) -> None:
    rel = Path("..")
    # path is typically $NDK/bin/<name>; toolchain_bin is
    # $NDK/toolchains/llvm/prebuilt/<tag>/bin
    try:
        rel = Path(os_relpath(toolchain_bin, path.parent))
    except ValueError:
        rel = Path("..") / "toolchains" / "llvm" / "prebuilt"
    write_executable(path, COMPAT.format(rel=rel.as_posix(), name=name))


def os_relpath(target: Path, start: Path) -> str:
    import os

    return os.path.relpath(target, start)
