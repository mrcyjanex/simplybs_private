"""Build compiler-rt builtins and libc++ for Android ABIs."""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

from layout import ABIS, MIN_API


def _run(cmd: list[str], cwd: Path | None = None) -> None:
    print("+", " ".join(cmd), flush=True)
    env = os.environ.copy()
    # The bootstrap clang in $NATIVEPREFIX/_ ships an LLVM CMake package that
    # points at missing llvm-tblgen. compiler-rt/libcxx must not pick it up.
    env.pop("LLVM_DIR", None)
    subprocess.check_call(cmd, cwd=cwd, env=env)


def _cmake_configure(build: Path, args: list[str]) -> None:
    if build.exists():
        shutil.rmtree(build)
    build.mkdir(parents=True)
    _run(["cmake", *args, "-B", str(build)])


def _first(paths: list[Path]) -> Path | None:
    for path in paths:
        if path.exists():
            return path
    return None


def collect_abi(install_prefix: Path, abi_name: str, dest: Path) -> None:
    info = ABIS[abi_name]
    lib_dest = dest / "lib"
    inc_dest = dest / "include" / "c++" / "v1"
    lib_dest.mkdir(parents=True, exist_ok=True)

    builtins = list(install_prefix.rglob("libclang_rt.builtins*.a"))
    if builtins:
        shutil.copy2(builtins[0], lib_dest / f"libclang_rt.builtins-{info['builtin']}.a")

    for name in (
        "libc++_shared.so",
        "libc++_static.a",
        "libc++abi.a",
        "libunwind.a",
        "libc++.so",
        "libc++.a",
    ):
        found = _first(list(install_prefix.rglob(name)))
        if found is not None:
            shutil.copy2(found, lib_dest / name)

    headers = _first(
        [
            install_prefix / "include" / "c++" / "v1",
            install_prefix / "include" / "c++v1",
        ]
        + list(install_prefix.rglob("c++/v1/__config"))
    )
    if headers is not None:
        if headers.name == "__config":
            headers = headers.parent
        if headers.is_dir():
            if inc_dest.exists():
                shutil.rmtree(inc_dest)
            shutil.copytree(headers, inc_dest, symlinks=True)


def build_builtins(
    llvm_src: Path,
    clang: Path,
    clangxx: Path,
    sysroot: Path,
    abi_name: str,
    api: int,
    dest: Path,
    jobs: int,
) -> None:
    info = ABIS[abi_name]
    target = f"{info['clang_triple']}{api}"
    extra = info.get("cflags", "")
    flags = f"--target={target} --sysroot={sysroot} -fPIC {extra}".strip()
    build = dest / "build-rt"
    install = dest / "install-rt"
    _cmake_configure(
        build,
        [
            "-G",
            "Ninja",
            "-S",
            str(llvm_src / "compiler-rt" / "lib" / "builtins"),
            "-DCMAKE_BUILD_TYPE=Release",
            f"-DCMAKE_C_COMPILER={clang}",
            f"-DCMAKE_CXX_COMPILER={clangxx}",
            f"-DCMAKE_ASM_COMPILER={clang}",
            f"-DCMAKE_C_COMPILER_TARGET={target}",
            f"-DCMAKE_CXX_COMPILER_TARGET={target}",
            f"-DCMAKE_ASM_COMPILER_TARGET={target}",
            f"-DCMAKE_SYSROOT={sysroot}",
            f"-DCMAKE_C_FLAGS={flags}",
            f"-DCMAKE_CXX_FLAGS={flags}",
            f"-DCMAKE_ASM_FLAGS={flags}",
            "-DCMAKE_TRY_COMPILE_TARGET_TYPE=STATIC_LIBRARY",
            "-DLLVM_RUNTIMES_BUILD=ON",
            "-DANDROID=1",
            "-DCOMPILER_RT_DEFAULT_TARGET_ONLY=ON",
            "-DCOMPILER_RT_BUILTINS_HIDE_SYMBOLS=ON",
            f"-DCMAKE_INSTALL_PREFIX={install}",
        ],
    )
    _run(["ninja", "-C", str(build), f"-j{jobs}"])
    _run(["ninja", "-C", str(build), "install"])


def build_libcxx(
    llvm_src: Path,
    clang: Path,
    clangxx: Path,
    sysroot: Path,
    abi_name: str,
    api: int,
    dest: Path,
    jobs: int,
    builtins: Path | None = None,
) -> None:
    info = ABIS[abi_name]
    target = f"{info['clang_triple']}{api}"
    extra = info.get("cflags", "")
    flags = f"--target={target} --sysroot={sysroot} -fPIC {extra}".strip()
    link_flags = flags
    if builtins is not None:
        link_flags = f"{flags} {builtins}"
    build = dest / "build-cxx"
    install = dest / "install-cxx"
    _cmake_configure(
        build,
        [
            "-G",
            "Ninja",
            "-S",
            str(llvm_src / "runtimes"),
            "-DLLVM_ENABLE_RUNTIMES=libunwind;libcxx;libcxxabi",
            "-DLLVM_RUNTIMES_BUILD=ON",
            "-DCMAKE_BUILD_TYPE=Release",
            f"-DCMAKE_C_COMPILER={clang}",
            f"-DCMAKE_CXX_COMPILER={clangxx}",
            f"-DCMAKE_C_COMPILER_TARGET={target}",
            f"-DCMAKE_CXX_COMPILER_TARGET={target}",
            f"-DCMAKE_SYSROOT={sysroot}",
            f"-DCMAKE_C_FLAGS={flags}",
            f"-DCMAKE_CXX_FLAGS={flags}",
            f"-DCMAKE_SHARED_LINKER_FLAGS={link_flags}",
            f"-DCMAKE_EXE_LINKER_FLAGS={link_flags}",
            "-DCMAKE_TRY_COMPILE_TARGET_TYPE=STATIC_LIBRARY",
            "-DLIBCXX_ENABLE_SHARED=ON",
            "-DLIBCXX_ENABLE_STATIC=ON",
            "-DLIBCXX_ENABLE_ABI_LINKER_SCRIPT=ON",
            "-DLIBCXX_HAS_PTHREAD_API=ON",
            "-DLIBCXX_INCLUDE_TESTS=OFF",
            "-DLIBCXX_INCLUDE_BENCHMARKS=OFF",
            "-DLIBCXX_ABI_VERSION=1",
            "-DLIBCXX_ABI_NAMESPACE=__ndk1",
            "-DLIBCXX_USE_COMPILER_RT=ON",
            "-DLIBCXXABI_ENABLE_SHARED=OFF",
            "-DLIBCXXABI_ENABLE_STATIC=ON",
            "-DLIBCXXABI_USE_LLVM_UNWINDER=ON",
            "-DLIBCXXABI_USE_COMPILER_RT=ON",
            "-DLIBUNWIND_ENABLE_SHARED=OFF",
            "-DLIBUNWIND_ENABLE_STATIC=ON",
            "-DLIBUNWIND_USE_COMPILER_RT=ON",
            f"-DCMAKE_INSTALL_PREFIX={install}",
        ],
    )
    _run(["ninja", "-C", str(build), f"-j{jobs}"])
    _run(["ninja", "-C", str(build), "install"])


def build_runtimes(
    llvm_src: Path,
    clang_prefix: Path,
    sysroot: Path,
    output: Path,
    api: int = MIN_API,
    abis: list[str] | None = None,
    jobs: int | None = None,
) -> Path:
    clang = clang_prefix / "bin" / "clang"
    clangxx = clang_prefix / "bin" / "clang++"
    if not clang.exists():
        raise FileNotFoundError(clang)
    if jobs is None:
        jobs = int(os.environ.get("NUM_CORES", "4"))
    if abis is None:
        abis = list(ABIS)
    output.mkdir(parents=True, exist_ok=True)
    for abi_name in abis:
        work = output / f".work-{abi_name}"
        staged = output / abi_name
        if staged.exists():
            shutil.rmtree(staged)
        staged.mkdir(parents=True)
        build_builtins(llvm_src, clang, clangxx, sysroot, abi_name, api, work, jobs)
        builtin_libs = list((work / "install-rt").rglob("libclang_rt.builtins*.a"))
        builtins = builtin_libs[0] if builtin_libs else None
        build_libcxx(
            llvm_src,
            clang,
            clangxx,
            sysroot,
            abi_name,
            api,
            work,
            jobs,
            builtins=builtins,
        )
        collect_abi(work / "install-rt", abi_name, staged)
        collect_abi(work / "install-cxx", abi_name, staged)
        shutil.rmtree(work, ignore_errors=True)
    return output
