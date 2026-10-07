"""Build compiler-rt builtins and libc++ for Android ABIs."""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

from layout import ABIS, MIN_API, abi_min_api, clang_has_arch


def _run(cmd: list[str], cwd: Path | None = None) -> None:
    print("+", " ".join(cmd), flush=True)
    env = os.environ.copy()
    # Bootstrap $NATIVEPREFIX/_/bin/sh is toybox; llvm config.guess needs bash.
    env.pop("LLVM_DIR", None)
    # Host export-env leaks -L$NATIVEPREFIX/lib into Android link lines.
    for key in ("LDFLAGS", "CFLAGS", "CXXFLAGS", "CPPFLAGS", "LIBRARY_PATH"):
        env.pop(key, None)
    bash = Path(env.get("NATIVEPREFIX", "")) / "bin" / "bash"
    if bash.exists():
        sh = bash.parent / "sh"
        if not sh.exists() and not sh.is_symlink():
            sh.symlink_to(bash)
        env["PATH"] = f"{bash.parent}{os.pathsep}{env.get('PATH', '')}"
    subprocess.check_call(cmd, cwd=cwd, env=env)


def _libgcc_file(clang: Path, target: str) -> Path:
    return Path(
        subprocess.check_output(
            [str(clang), f"--target={target}", "-print-libgcc-file-name"],
            text=True,
        ).strip()
    )


def _install_builtins_for_clang(clang: Path, builtins: Path, target: str) -> None:
    wanted = _libgcc_file(clang, target)
    wanted.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(builtins, wanted)


def _install_lib_for_clang(clang: Path, lib: Path, target: str) -> None:
    dest = _libgcc_file(clang, target).parent
    dest.mkdir(parents=True, exist_ok=True)
    shutil.copy2(lib, dest / lib.name)


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
    lib_dest = dest / "lib"
    inc_dest = dest / "include" / "c++" / "v1"
    lib_dest.mkdir(parents=True, exist_ok=True)

    for lib in install_prefix.rglob("libclang_rt.*"):
        if lib.suffix in {".a", ".so", ".o"} or ".so." in lib.name:
            shutil.copy2(lib, lib_dest / lib.name)

    for name in (
        "libc++_shared.so",
        "libc++_static.a",
        "libc++abi.a",
        "libc++experimental.a",
        "libunwind.a",
        "libc++.so",
        "libc++.a",
        "libcompiler_rt-extras.a",
    ):
        found = _first(list(install_prefix.rglob(name)))
        if found is not None:
            shutil.copy2(found, lib_dest / name)

    shared = lib_dest / "libc++_shared.so"
    script = lib_dest / "libc++.so"
    if shared.exists() and not script.exists():
        script.write_text("INPUT(-lc++_shared)\n")
    static = lib_dest / "libc++_static.a"
    static_script = lib_dest / "libc++.a"
    if static.exists() and not static_script.exists():
        static_script.write_text("INPUT(-lc++_static -lc++abi)\n")

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


def _crt_cmake_args(
    llvm_src: Path,
    clang: Path,
    clangxx: Path,
    sysroot: Path,
    target: str,
    flags: str,
    install: Path,
) -> list[str]:
    return [
        "-G",
        "Ninja",
        "-S",
        str(llvm_src / "compiler-rt"),
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
        "-DANDROID=1",
        # Bootstrap $NATIVEPREFIX/_ ships a partial LLVMConfig (no llvm-tblgen).
        # Skip it so compiler-rt mocks AddLLVM from this monorepo.
        "-DCMAKE_DISABLE_FIND_PACKAGE_LLVM=ON",
        "-DCOMPILER_RT_DEFAULT_TARGET_ONLY=ON",
        "-DCOMPILER_RT_BUILTINS_HIDE_SYMBOLS=ON",
        "-DCOMPILER_RT_BUILD_XRAY=OFF",
        "-DCOMPILER_RT_BUILD_MEMPROF=OFF",
        "-DCOMPILER_RT_BUILD_ORC=OFF",
        "-DCOMPILER_RT_BUILD_CTX_PROFILE=OFF",
        "-DCOMPILER_RT_INCLUDE_TESTS=OFF",
        f"-DCMAKE_INSTALL_PREFIX={install}",
    ]


def _build_libunwind(
    llvm_src: Path,
    clang: Path,
    clangxx: Path,
    sysroot: Path,
    target: str,
    flags: str,
    dest: Path,
    jobs: int,
) -> None:
    # The driver would otherwise ask for libunwind while it is being built.
    unwind_flags = f"{flags} --unwindlib=none"
    install = dest / "install-unwind"
    build = dest / "build-unwind"
    _cmake_configure(
        build,
        [
            "-G",
            "Ninja",
            "-S",
            str(llvm_src / "runtimes"),
            "-DLLVM_ENABLE_RUNTIMES=libunwind",
            "-DCMAKE_BUILD_TYPE=Release",
            "-DCMAKE_TRY_COMPILE_TARGET_TYPE=STATIC_LIBRARY",
            f"-DCMAKE_C_COMPILER={clang}",
            f"-DCMAKE_CXX_COMPILER={clangxx}",
            f"-DCMAKE_ASM_COMPILER={clang}",
            f"-DCMAKE_C_COMPILER_TARGET={target}",
            f"-DCMAKE_CXX_COMPILER_TARGET={target}",
            f"-DCMAKE_ASM_COMPILER_TARGET={target}",
            f"-DCMAKE_SYSROOT={sysroot}",
            f"-DCMAKE_C_FLAGS={unwind_flags}",
            f"-DCMAKE_CXX_FLAGS={unwind_flags}",
            f"-DCMAKE_ASM_FLAGS={unwind_flags}",
            f"-DCMAKE_EXE_LINKER_FLAGS={unwind_flags}",
            f"-DCMAKE_SHARED_LINKER_FLAGS={unwind_flags}",
            "-DLIBUNWIND_USE_COMPILER_RT=ON",
            "-DLIBUNWIND_ENABLE_SHARED=OFF",
            "-DLIBUNWIND_ENABLE_STATIC=ON",
            "-DLIBUNWIND_INCLUDE_TESTS=OFF",
            f"-DCMAKE_INSTALL_PREFIX={install}",
        ],
    )
    _run(["ninja", "-C", str(build), f"-j{jobs}", "install"])
    libs = list(install.rglob("libunwind.a"))
    if not libs:
        raise FileNotFoundError(install / "libunwind.a")
    _install_lib_for_clang(clang, libs[0], target)


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
    install = dest / "install-rt"
    base = _crt_cmake_args(llvm_src, clang, clangxx, sysroot, target, flags, install)
    # android-clang defaults to compiler-rt, so an executable try_compile looks
    # for libclang_rt.builtins before this build can produce it.
    builtins_build = dest / "build-rt-builtins"
    _cmake_configure(
        builtins_build,
        base
        + [
            "-DCMAKE_TRY_COMPILE_TARGET_TYPE=STATIC_LIBRARY",
            "-DCOMPILER_RT_BUILD_BUILTINS=ON",
            "-DCOMPILER_RT_BUILD_SANITIZERS=OFF",
            "-DCOMPILER_RT_BUILD_LIBFUZZER=OFF",
            "-DCOMPILER_RT_BUILD_PROFILE=OFF",
        ],
    )
    _run(["ninja", "-C", str(builtins_build), f"-j{jobs}", "install"])
    builtin_libs = list(install.rglob("libclang_rt.builtins*.a"))
    if not builtin_libs:
        raise FileNotFoundError(install / "libclang_rt.builtins.a")
    _install_builtins_for_clang(clang, builtin_libs[0], target)
    # android-clang links every executable with -l:libunwind.a. Build that
    # archive before sanitizers so their compiler test can link.
    _build_libunwind(llvm_src, clang, clangxx, sysroot, target, flags, dest, jobs)
    rest_build = dest / "build-rt"
    # The sysroot libc++.so script points at libc++_shared, which this stage
    # does not build. Keep the C++ compiler test off that library.
    nostd = f"{flags} -nostdlib++"
    _cmake_configure(
        rest_build,
        base
        + [
            "-DCOMPILER_RT_BUILD_BUILTINS=OFF",
            "-DCOMPILER_RT_BUILD_SANITIZERS=ON",
            "-DCOMPILER_RT_BUILD_LIBFUZZER=ON",
            "-DCOMPILER_RT_BUILD_PROFILE=ON",
            f"-DCMAKE_EXE_LINKER_FLAGS={nostd}",
            f"-DCMAKE_SHARED_LINKER_FLAGS={nostd}",
        ],
    )
    _run(["ninja", "-C", str(rest_build), f"-j{jobs}", "install"])


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
    link_flags = f"{flags} -nostdlib++"
    if builtins is not None:
        _install_builtins_for_clang(clang, builtins, target)
    build = dest / "build-cxx"
    install = dest / "install-cxx"
    _cmake_configure(
        build,
        [
            "-G",
            "Ninja",
            "-S",
            str(llvm_src / "runtimes"),
            f"-C{llvm_src / 'libcxx' / 'cmake' / 'caches' / 'AndroidNDK.cmake'}",
            "-DLLVM_ENABLE_RUNTIMES=libunwind;libcxx;libcxxabi",
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
            f"-DCMAKE_SHARED_LINKER_FLAGS={link_flags}",
            f"-DCMAKE_EXE_LINKER_FLAGS={link_flags}",
            "-DLIBCXXABI_USE_LLVM_UNWINDER=ON",
            "-DLIBCXX_USE_COMPILER_RT=ON",
            "-DLIBCXXABI_USE_COMPILER_RT=ON",
            "-DLIBCXX_INCLUDE_TESTS=OFF",
            "-DLIBCXX_INCLUDE_BENCHMARKS=OFF",
            "-DLIBCXX_ENABLE_EXPERIMENTAL_LIBRARY=ON",
            "-DLIBUNWIND_ENABLE_SHARED=OFF",
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
    requested = list(ABIS) if abis is None else list(abis)
    implicit = abis is None
    output.mkdir(parents=True, exist_ok=True)
    for abi_name in requested:
        info = ABIS[abi_name]
        if not clang_has_arch(clang, info["arch"]):
            msg = f"clang has no backend for {info['arch']} ({abi_name})"
            if not implicit:
                raise RuntimeError(msg)
            print("WARNING:", msg, "- skipped until android-clang includes that target", flush=True)
            continue
        abi_api = max(api, abi_min_api(abi_name))
        work = output / f".work-{abi_name}"
        staged = output / abi_name
        if staged.exists():
            shutil.rmtree(staged)
        staged.mkdir(parents=True)
        build_builtins(llvm_src, clang, clangxx, sysroot, abi_name, abi_api, work, jobs)
        builtin_libs = list((work / "install-rt").rglob("libclang_rt.builtins*.a"))
        builtins = builtin_libs[0] if builtin_libs else None
        build_libcxx(
            llvm_src,
            clang,
            clangxx,
            sysroot,
            abi_name,
            abi_api,
            work,
            jobs,
            builtins=builtins,
        )
        collect_abi(work / "install-rt", abi_name, staged)
        collect_abi(work / "install-cxx", abi_name, staged)
        shutil.rmtree(work, ignore_errors=True)
    return output
