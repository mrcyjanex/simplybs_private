"""Merge clang, sysroot skeleton, and runtimes into an NDK zip layout."""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

from cmake_patch import patch_cmake_host_tag
from layout import ABIS, Abi, api_levels, clang_bin, lib_dir, sysroot_path, toolchain_root
from wrappers import write_clang_wrappers, write_compat_wrapper, write_ld_wrapper

CLANG_TOOL_LINKS = {
    "ar": "llvm-ar",
    "ranlib": "llvm-ranlib",
    "nm": "llvm-nm",
    "strip": "llvm-strip",
    "objcopy": "llvm-objcopy",
    "objdump": "llvm-objdump",
    "readelf": "llvm-readelf",
    "as": "clang",
}


def _copytree(src: Path, dest: Path) -> None:
    if dest.exists():
        shutil.rmtree(dest)
    shutil.copytree(src, dest, symlinks=True)


def _copy_file(src: Path, dest: Path) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    if dest.exists() or dest.is_symlink():
        dest.unlink()
    shutil.copy2(src, dest, follow_symlinks=True)


def _symlink(dest: Path, target: str) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    if dest.exists() or dest.is_symlink():
        dest.unlink()
    dest.symlink_to(target)


def clang_resource_dir(clang: Path) -> Path:
    out = subprocess.check_output([str(clang), "-print-resource-dir"], text=True)
    return Path(out.strip())


def copy_clang_into_toolchain(clang_prefix: Path, toolchain: Path) -> Path:
    src_bin = clang_prefix / "bin"
    dest_bin = toolchain / "bin"
    dest_bin.mkdir(parents=True, exist_ok=True)
    if not src_bin.is_dir():
        raise FileNotFoundError(f"clang prefix has no bin/: {clang_prefix}")
    for src in src_bin.iterdir():
        if src.is_dir():
            continue
        _copy_file(src, dest_bin / src.name)

    clang = dest_bin / "clang"
    if not clang.exists():
        raise FileNotFoundError(f"clang missing after copy: {clang}")
    if not (dest_bin / "clang++").exists():
        _symlink(dest_bin / "clang++", "clang")
    if (dest_bin / "ld.lld").exists():
        _symlink(dest_bin / "ld", "ld.lld")
        _symlink(dest_bin / "lld", "ld.lld")
    for name, target in CLANG_TOOL_LINKS.items():
        if (dest_bin / target).exists() and not (dest_bin / name).exists():
            _symlink(dest_bin / name, target)

    src_resource = clang_prefix / "lib" / "clang"
    dest_resource_root = toolchain / "lib" / "clang"
    if src_resource.is_dir():
        _copytree(src_resource, dest_resource_root)
    else:
        try:
            resource = clang_resource_dir(clang)
        except (OSError, subprocess.CalledProcessError):
            resource = None
        if resource is not None and resource.is_dir():
            _copytree(resource, dest_resource_root / resource.name)
    return dest_bin


def overlay_runtimes(runtimes: Path, toolchain: Path, sysroot: Path) -> None:
    if not runtimes.is_dir():
        return
    resource_root = toolchain / "lib" / "clang"
    version_dirs = [p for p in resource_root.iterdir() if p.is_dir()] if resource_root.is_dir() else []
    linux_dir = (version_dirs[0] / "lib" / "linux") if version_dirs else (toolchain / "lib" / "linux")
    linux_dir.mkdir(parents=True, exist_ok=True)

    for abi_name, info in ABIS.items():
        abi_dir = runtimes / abi_name
        if not abi_dir.is_dir():
            continue
        for builtin in (abi_dir / "lib").glob("libclang_rt.builtins*.a"):
            dest = linux_dir / f"libclang_rt.builtins-{info['builtin']}.a"
            _copy_file(builtin, dest)
        lib_triple = info["lib_triple"]
        dest_lib = lib_dir(sysroot, lib_triple)
        dest_lib.mkdir(parents=True, exist_ok=True)
        for name in (
            "libc++_shared.so",
            "libc++_static.a",
            "libc++abi.a",
            "libunwind.a",
            "libc++.so",
            "libc++.a",
        ):
            src = abi_dir / "lib" / name
            if src.exists():
                _copy_file(src, dest_lib / name)
        headers = abi_dir / "include" / "c++" / "v1"
        if headers.is_dir():
            dest_headers = sysroot / "usr" / "include" / "c++" / "v1"
            _copytree(headers, dest_headers)


def write_all_wrappers(bin_dir: Path, sysroot: Path) -> list[int]:
    levels = api_levels(sysroot) or [21]
    for info in ABIS.values():
        write_clang_wrappers(bin_dir, info["clang_triple"], levels)
        write_ld_wrapper(bin_dir, info["lib_triple"])
    return levels


def install_compat_bin(ndk: Path, tag: str, clang_triple: str, api: int) -> None:
    dest = ndk / "bin"
    src = clang_bin(ndk, tag)
    names = [
        f"{clang_triple}{api}-clang",
        f"{clang_triple}{api}-clang++",
        f"{clang_triple}-clang",
        f"{clang_triple}-clang++",
        "clang",
        "clang++",
        "llvm-ar",
        "llvm-ranlib",
        "llvm-nm",
        "llvm-strip",
        "llvm-as",
        "llvm-objcopy",
        "llvm-objdump",
        "llvm-readelf",
        "ar",
        "ranlib",
        "nm",
        "strip",
        "as",
        "ld",
        "lld",
    ]
    abi = Abi.from_clang_triple(clang_triple)
    names.append(f"{abi.lib_triple}-ld")
    for name in names:
        if (src / name).exists():
            write_compat_wrapper(dest / name, src, name)


def copy_prefix_libs(sysroot: Path, lib_triple: str, api: int, dest: Path) -> None:
    dest.mkdir(parents=True, exist_ok=True)
    generic = lib_dir(sysroot, lib_triple)
    versioned = lib_dir(sysroot, lib_triple, api)
    for folder in (generic, versioned):
        if not folder.is_dir():
            continue
        for src in folder.iterdir():
            if src.is_dir():
                continue
            if src.suffix in {".a", ".so", ".o"}:
                _copy_file(src, dest / src.name)


def install(
    skeleton: Path,
    clang_prefix: Path,
    ndk_out: Path,
    host_tag: str,
    runtimes: Path | None = None,
    prefix_lib_dir: Path | None = None,
    target_triple: str | None = None,
    api: int = 21,
) -> Path:
    _copytree(skeleton, ndk_out)
    toolchain = toolchain_root(ndk_out, host_tag)
    toolchain.mkdir(parents=True, exist_ok=True)
    sysroot = sysroot_path(ndk_out, host_tag)
    if not sysroot.is_dir():
        raise FileNotFoundError(f"skeleton sysroot missing: {sysroot}")

    dest_bin = copy_clang_into_toolchain(clang_prefix, toolchain)
    if runtimes is not None:
        overlay_runtimes(runtimes, toolchain, sysroot)
    levels = write_all_wrappers(dest_bin, sysroot)
    if api not in levels:
        write_clang_wrappers(dest_bin, ABIS["arm64-v8a"]["clang_triple"], [api])
    patch_cmake_host_tag(ndk_out, host_tag)

    if target_triple:
        abi = Abi.from_clang_triple(target_triple)
        install_compat_bin(ndk_out, host_tag, abi.clang_triple, api)
        if prefix_lib_dir is not None:
            copy_prefix_libs(sysroot, abi.lib_triple, api, prefix_lib_dir)

    return ndk_out
