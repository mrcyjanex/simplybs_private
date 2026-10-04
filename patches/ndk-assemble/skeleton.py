"""Strip a stock NDK zip tree down to a sysroot + scripts skeleton."""

from __future__ import annotations

import shutil
from pathlib import Path

from layout import ZIP_HOST_TAGS, toolchain_root


def detect_zip_host_tag(ndk: Path) -> str:
    prebuilt = ndk / "toolchains" / "llvm" / "prebuilt"
    if not prebuilt.is_dir():
        raise FileNotFoundError(f"missing {prebuilt}")
    tags = sorted(p.name for p in prebuilt.iterdir() if p.is_dir())
    for tag in ZIP_HOST_TAGS:
        if tag in tags:
            return tag
    if len(tags) == 1:
        return tags[0]
    raise RuntimeError(f"could not detect NDK host tag in {prebuilt}: {tags}")


def relocate_host_tag(ndk: Path, dest_tag: str) -> Path:
    src_tag = detect_zip_host_tag(ndk)
    src = toolchain_root(ndk, src_tag)
    dest = toolchain_root(ndk, dest_tag)
    if src == dest:
        return dest
    dest.parent.mkdir(parents=True, exist_ok=True)
    if dest.exists():
        shutil.rmtree(dest)
    shutil.move(str(src), str(dest))
    return dest


def strip_compiler(toolchain: Path) -> None:
    for name in ("bin", "lib", "lib64", "python3", "runtimes_ndk_cxx", "android_libc++", "musl"):
        path = toolchain / name
        if path.exists():
            shutil.rmtree(path)
    for pattern in ("manifest_*.xml", "clang_source_info.md"):
        for path in toolchain.glob(pattern):
            path.unlink()


def prepare_skeleton(input_ndk: Path, output: Path, host_tag: str) -> Path:
    if output.exists():
        shutil.rmtree(output)
    shutil.copytree(input_ndk, output, symlinks=True)
    toolchain = relocate_host_tag(output, host_tag)
    strip_compiler(toolchain)
    return output
