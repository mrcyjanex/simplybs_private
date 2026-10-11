"""Strip a stock NDK zip tree down to a sysroot + scripts skeleton."""

from __future__ import annotations

import shutil
from pathlib import Path

from layout import ABIS, ZIP_HOST_TAGS, prune_sysroot_abis, toolchain_root


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


def _is_linker_script(path: Path) -> bool:
    data = path.read_bytes()[:256]
    if data.startswith(b"\x7fELF") or data.startswith(b"!<arch>\n"):
        return False
    text = data.decode("ascii", "replace")
    return "INPUT(" in text or "GROUP(" in text or "OUTPUT_FORMAT(" in text


def is_binary_artifact(path: Path) -> bool:
    if not path.is_file():
        return False
    data = path.read_bytes()[:8]
    if data.startswith(b"\x7fELF") or data.startswith(b"!<arch>\n"):
        return True
    if path.suffix.lower() in {".o", ".obj", ".a", ".so", ".dylib", ".dll", ".bc"}:
        return not _is_linker_script(path)
    return False


def strip_binaries(root: Path) -> None:
    """Drop every ELF/archive copied from the zip. Linker scripts (text) stay."""
    for path in list(root.rglob("*")):
        if is_binary_artifact(path):
            path.unlink()


# Zip copies of host tools simplybs already ships outside the NDK.
DROP_HOST_DIRS = (
    "prebuilt",  # GNU make, yasm
    "shader-tools",
)
DROP_HOST_FILES = (
    "ndk-lldb",
)


def strip_host_tools(ndk: Path) -> None:
    """Drop zip copies of host tools that live outside the NDK in simplybs."""
    for name in DROP_HOST_DIRS:
        path = ndk / name
        if path.exists():
            shutil.rmtree(path)
    for name in DROP_HOST_FILES:
        path = ndk / name
        if path.is_file() or path.is_symlink():
            path.unlink()


def patch_host_tag_scripts(ndk: Path, host_tag: str) -> None:
    """Rewrite zip host-tag paths in text scripts to this build's host tag."""
    for path in ndk.rglob("*"):
        if not path.is_file() or path.is_symlink():
            continue
        if path.suffix.lower() in {".so", ".a", ".o", ".pyz", ".pyc"}:
            continue
        try:
            data = path.read_bytes()
        except OSError:
            continue
        if b"\0" in data[:256]:
            continue
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            continue
        new = text
        for old in ZIP_HOST_TAGS:
            new = new.replace(f"toolchains/llvm/prebuilt/{old}", f"toolchains/llvm/prebuilt/{host_tag}")
        if new != text:
            path.write_text(new)


def prepare_skeleton(input_ndk: Path, output: Path, host_tag: str) -> Path:
    if output.exists():
        shutil.rmtree(output)
    shutil.copytree(input_ndk, output, symlinks=True)
    toolchain = relocate_host_tag(output, host_tag)
    strip_compiler(toolchain)
    strip_host_tools(output)
    prune_sysroot_abis(toolchain / "sysroot", list(ABIS))
    patch_host_tag_scripts(output, host_tag)
    strip_binaries(output)
    return output
