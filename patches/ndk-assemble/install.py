"""Merge clang, sysroot skeleton, and runtimes into an NDK zip layout."""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

from cmake_patch import patch_cmake_host_tag
from layout import ABIS, Abi, api_levels, lib_dir, sysroot_path, toolchain_root
from wrappers import write_clang_wrappers, write_ld_wrapper

HOST_LIB_GLOBS = ("libc++.so*", "libc++abi.so*", "libunwind.so*")
CLANG_TOOL_LINKS = {
    "ar": "llvm-ar",
    "ranlib": "llvm-ranlib",
    "nm": "llvm-nm",
    "strip": "llvm-strip",
    "objcopy": "llvm-objcopy",
    "objdump": "llvm-objdump",
    "readelf": "llvm-readelf",
    "readobj": "llvm-readobj",
    "as": "llvm-as",
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
    if (dest_bin / "llvm-readobj").exists() and not (dest_bin / "llvm-readelf").exists():
        _symlink(dest_bin / "llvm-readelf", "llvm-readobj")
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
    copy_host_libs([clang_prefix / "lib"], toolchain)
    return dest_bin


def copy_host_libs(sources: list[Path], toolchain: Path) -> None:
    dest = toolchain / "lib"
    dest.mkdir(parents=True, exist_ok=True)
    for src_dir in sources:
        if src_dir is None or not src_dir.is_dir():
            continue
        for pattern in HOST_LIB_GLOBS:
            for src in src_dir.glob(pattern):
                if src.is_dir():
                    continue
                _copy_file(src, dest / src.name)


def write_libatomic(toolchain: Path, sysroot: Path) -> None:
    """compiler-rt builtins contain the atomic helpers. The NDK still ships an
    empty libatomic.a so -latomic, added by CMake's Android platform, resolves.
    The driver searches the sysroot library directories, not the resource dir.
    """
    resource_root = toolchain / "lib" / "clang"
    if not resource_root.is_dir():
        raise FileNotFoundError(resource_root)
    version_dirs = sorted(p for p in resource_root.iterdir() if p.is_dir())
    if not version_dirs:
        raise FileNotFoundError(resource_root)
    linux_dir = version_dirs[0] / "lib" / "linux"
    linux_dir.mkdir(parents=True, exist_ok=True)
    dest = linux_dir / "libatomic.a"
    if dest.exists() or dest.is_symlink():
        dest.unlink()
    ar = toolchain / "bin" / "llvm-ar"
    subprocess.check_call([str(ar), "rc", str(dest)])
    libroot = sysroot / "usr" / "lib"
    if not libroot.is_dir():
        return
    for triple_dir in libroot.iterdir():
        if not triple_dir.is_dir():
            continue
        shutil.copy2(dest, triple_dir / "libatomic.a")
        for child in triple_dir.iterdir():
            if child.is_dir() and child.name.isdigit():
                shutil.copy2(dest, child / "libatomic.a")


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
        for rt in (abi_dir / "lib").glob("libclang_rt.*"):
            _copy_file(rt, linux_dir / rt.name)
        lib_triple = info["lib_triple"]
        dest_lib = lib_dir(sysroot, lib_triple)
        dest_lib.mkdir(parents=True, exist_ok=True)
        for name in (
            "libc++_shared.so",
            "libc++_static.a",
            "libc++abi.a",
            "libc++experimental.a",
            "libunwind.a",
            "libc++.so",
            "libc++.a",
            "libcompiler_rt-extras.a",
            "libc.a",
            "libm.a",
            "libdl.a",
            "libz.a",
            "libstdc++.a",
        ):
            src = abi_dir / "lib" / name
            if src.exists():
                _copy_file(src, dest_lib / name)
        for child in dest_lib.iterdir():
            if child.is_dir() and child.name.isdigit():
                for script in ("libc++.so", "libc++.a"):
                    src = dest_lib / script
                    if src.exists():
                        _copy_file(src, child / script)
        headers = abi_dir / "include" / "c++" / "v1"
        if headers.is_dir():
            dest_headers = sysroot / "usr" / "include" / "c++" / "v1"
            _copytree(headers, dest_headers)


def write_all_wrappers(bin_dir: Path, sysroot: Path, clang_triple: str | None = None) -> list[int]:
    levels = api_levels(sysroot) or [21]
    triples = [clang_triple] if clang_triple else [info["clang_triple"] for info in ABIS.values()]
    for triple in triples:
        abi = Abi.from_clang_triple(triple)
        write_clang_wrappers(bin_dir, abi.clang_triple, levels)
        write_ld_wrapper(bin_dir, abi.lib_triple)
    return levels


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
    host_lib_dir: Path | None = None,
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
    if host_lib_dir is not None:
        copy_host_libs([host_lib_dir], toolchain)
    if runtimes is not None:
        overlay_runtimes(runtimes, toolchain, sysroot)
    write_libatomic(toolchain, sysroot)
    levels = write_all_wrappers(dest_bin, sysroot, clang_triple=target_triple)
    if api not in levels and target_triple:
        write_clang_wrappers(dest_bin, Abi.from_clang_triple(target_triple).clang_triple, [api])
    elif api not in levels:
        write_clang_wrappers(dest_bin, ABIS["arm64-v8a"]["clang_triple"], [api])
    patch_cmake_host_tag(ndk_out, host_tag)

    if target_triple and prefix_lib_dir is not None:
        abi = Abi.from_clang_triple(target_triple)
        copy_prefix_libs(sysroot, abi.lib_triple, api, prefix_lib_dir)

    return ndk_out
