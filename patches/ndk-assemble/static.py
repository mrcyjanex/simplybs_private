"""Build NDK static archives (libz, libdl, libstdc++, libc, libm, compiler-rt extras) from source."""

from __future__ import annotations

import os
import shutil
import subprocess
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

from bp import index_blueprints, module_cflags, module_includes, resolve_module_srcs
from layout import ABIS, abi_min_api
from unicode import extract_ucd, generate_icu4x_c

ARCH_DIR = {
    "arm64-v8a": "arm64",
    "armeabi-v7a": "arm",
    "x86_64": "x86_64",
}

# Compile static bionic against the platform API so headers expose the full ABI.
PLATFORM_API = 10000

LIBC_MODULES = (
    "libc_sources_static",
    "libc_bionic",
    "libc_freebsd",
    "libc_freebsd_large_stack",
    "libc_freebsd_ldexp",
    "libc_netbsd",
    "libc_openbsd",
    "libc_openbsd_large_stack",
    "libc_gdtoa",
    "libc_fortify",
    "libc_tzcode",
    "libc_dns",
    "libc_bootstrap",
    "libc_init_static",
    "libc_unwind_static",
    "libc_aeabi",
    "libc_static_dispatch",
    "libc_syscalls",
    "libasync_safe",
    "libsystemproperties",
)

ARM_OPT_MODULES = (
    "libarm-optimized-routines-string",
)

BIONIC_CFLAGS = [
    "-D_LIBC=1",
    "-D_BIONIC=1",
    "-D__BIONIC_LP32_USE_STAT64",
    "-DLIBC_STATIC",
    "-fno-builtin",
    "-fno-exceptions",
    "-fno-rtti",
    "-std=gnu17",
    "-fPIC",
    "-O2",
    "-funwind-tables",
]

SCUDO_SKIP = {
    "fuchsia.cpp",
    "mem_map_fuchsia.cpp",
    "condition_variable_fuchsia.cpp",
    "report_fuchsia.cpp",
    "trusty.cpp",
    "wrappers_c.cpp",
    "wrappers_cpp.cpp",
}

SCUDO_SRCS = (
    "checksum.cpp",
    "common.cpp",
    "condition_variable_linux.cpp",
    "crc32_hw.cpp",
    "flags_parser.cpp",
    "flags.cpp",
    "linux.cpp",
    "mem_map.cpp",
    "mem_map_linux.cpp",
    "release.cpp",
    "report.cpp",
    "report_linux.cpp",
    "string_utils.cpp",
    "timing.cpp",
    "wrappers_c_bionic.cpp",
)


def _run(cmd: list[str], cwd: Path | None = None, env: dict[str, str] | None = None) -> None:
    print("+", " ".join(cmd), flush=True)
    subprocess.check_call(cmd, cwd=cwd, env=env)


def _clang_target(clang: Path, sysroot: Path, target: str, extra: list[str] | None = None) -> list[str]:
    cmd = [
        str(clang),
        f"--target={target}",
        f"--sysroot={sysroot}",
        "-fuse-ld=lld",
        "-nostdlib",
        "-fPIC",
        "-O2",
    ]
    if extra:
        cmd.extend(extra)
    return cmd


def _archive(llvm_ar: Path, dest: Path, objs: list[Path]) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    if dest.exists():
        dest.unlink()
    # llvm-ar has an argv limit; pack via an rsp file.
    rsp = dest.with_suffix(".rsp")
    rsp.write_text("\n".join(str(o) for o in objs) + "\n")
    _run([str(llvm_ar), "rcs", str(dest), f"@{rsp}"])
    rsp.unlink(missing_ok=True)


def generate_android_ids(header: Path, dest: Path) -> None:
    import re

    ids: list[tuple[str, int]] = []
    for line in header.read_text().splitlines():
        m = re.match(r"#define\s+AID_([A-Z0-9_]+)\s+(\d+)", line)
        if not m:
            continue
        field, num = m.group(1), int(m.group(2))
        if field.endswith(("_START", "_END", "_OFFSET")):
            continue
        ids.append((field.lower(), num))
    body = ",\n".join(f'  {{"{name}", {aid}}}' for name, aid in ids)
    dest.write_text(
        "struct android_id_info { const char name[32]; unsigned aid; };\n"
        f"static const struct android_id_info android_ids[] = {{\n{body}\n}};\n"
        f"#define android_id_count {len(ids)}\n"
    )


def gensyscalls(bionic: Path, arch: str, dest: Path) -> Path:
    script = bionic / "libc" / "tools" / "gensyscalls.py"
    src = bionic / "libc" / "SYSCALLS.TXT"
    dest.parent.mkdir(parents=True, exist_ok=True)
    text = subprocess.check_output(["python3", str(script), arch, str(src)], text=True)
    dest.write_text(text)
    return dest


def _drop_werror(flags: list[str]) -> list[str]:
    return [f for f in flags if f != "-Werror" and not f.startswith("-Werror=")]


def _flatten_flags(flags: list[str]) -> list[str]:
    """Soong stores `-include foo.h` as one string; clang wants two argv words."""
    out: list[str] = []
    for flag in flags:
        if flag.startswith("-include ") or flag.startswith("-imacros ") or flag.startswith("-Xclang "):
            opt, _, rest = flag.partition(" ")
            out.extend([opt, rest])
        else:
            out.append(flag)
    return out


def _compile_one(
    clang: Path,
    sysroot: Path,
    target: str,
    src: Path,
    obj: Path,
    includes: list[str],
    extra: list[str],
) -> None:
    obj.parent.mkdir(parents=True, exist_ok=True)
    flags = _flatten_flags(_drop_werror(list(extra)))
    lang: list[str] = []
    incs = list(includes)
    if src.suffix in {".S", ".s"}:
        lang = ["-x", "assembler-with-cpp"]
        flags = [
            f
            for f in flags
            if not f.startswith("-std=") and f not in {"-fno-exceptions", "-fno-rtti", "-fno-builtin"}
        ]
        flags.append("-D__ASSEMBLY__")
        incs = [i for i in incs if not i.endswith("/libc/include")]
    elif src.suffix in {".cpp", ".cc", ".cxx"}:
        lang = ["-x", "c++"]
        flags = [f if not f.startswith("-std=") else "-std=gnu++20" for f in flags]
        flags.append("-nostdinc++")
    cmd = _clang_target(clang, sysroot, target, flags) + lang + incs + ["-c", str(src), "-o", str(obj)]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(f"compile failed: {src}\n{proc.stderr}")


def _jobs() -> int:
    return max(1, int(os.environ.get("NUM_CORES", os.cpu_count() or 4)))


def _compile_many(
    clang: Path,
    sysroot: Path,
    target: str,
    items: list[tuple[Path, Path, list[str], list[str]]],
) -> list[Path]:
    objs: list[Path] = []
    errors: list[str] = []
    if not items:
        return objs

    def work(item: tuple[Path, Path, list[str], list[str]]) -> Path:
        src, obj, includes, extra = item
        _compile_one(clang, sysroot, target, src, obj, includes, extra)
        return obj

    with ThreadPoolExecutor(max_workers=_jobs()) as pool:
        futs = {pool.submit(work, item): item[0] for item in items}
        for fut in as_completed(futs):
            src = futs[fut]
            try:
                objs.append(fut.result())
            except Exception as exc:
                errors.append(f"{src}: {exc}")
    if errors:
        raise RuntimeError(f"{len(errors)} compile failures:\n" + "\n".join(errors[:40]))
    return objs


def build_libdl(clang: Path, llvm_ar: Path, bionic: Path, sysroot: Path, target: str, dest: Path) -> None:
    src = bionic / "libdl" / "libdl_static.cpp"
    obj = dest.with_suffix(".o")
    includes = [f"-I{bionic / 'libc'}"]
    _compile_one(clang, sysroot, target, src, obj, includes, ["-D_LIBC=1"])
    _archive(llvm_ar, dest, [obj])
    obj.unlink(missing_ok=True)


def build_libstdcxx(
    clang: Path,
    llvm_ar: Path,
    bionic: Path,
    sysroot: Path,
    target: str,
    dest: Path,
) -> None:
    includes = [
        f"-I{bionic / 'libstdc++' / 'include'}",
        f"-I{bionic / 'libc'}",
        f"-I{bionic / 'libc' / 'async_safe' / 'include'}",
    ]
    srcs = [
        bionic / "libc" / "bionic" / "new.cpp",
        bionic / "libc" / "bionic" / "__cxa_pure_virtual.cpp",
        bionic / "libc" / "bionic" / "__cxa_guard.cpp",
        bionic / "libc" / "async_safe" / "async_safe_log.cpp",
    ]
    objs: list[Path] = []
    work = dest.parent / ".libstdcxx"
    work.mkdir(parents=True, exist_ok=True)
    for src in srcs:
        obj = work / (src.name + ".o")
        _compile_one(clang, sysroot, target, src, obj, includes, ["-D_LIBC=1", "-nostdinc++"])
        objs.append(obj)
    _archive(llvm_ar, dest, objs)
    shutil.rmtree(work, ignore_errors=True)


ZLIB_SRCS = (
    "adler32.c",
    "compress.c",
    "crc32.c",
    "deflate.c",
    "gzclose.c",
    "gzlib.c",
    "gzread.c",
    "gzwrite.c",
    "infback.c",
    "inflate.c",
    "inftrees.c",
    "inffast.c",
    "trees.c",
    "uncompr.c",
    "zutil.c",
)


def build_libz(
    clang: Path,
    llvm_ar: Path,
    zlib_src: Path,
    sysroot: Path,
    target: str,
    dest: Path,
    extra_cflags: str = "",
) -> None:
    work = dest.parent / ".zlib"
    if work.exists():
        shutil.rmtree(work)
    work.mkdir(parents=True)
    extra = ["-D_LARGEFILE64_SOURCE=1", "-D_FILE_OFFSET_BITS=64"]
    if extra_cflags:
        extra.extend(extra_cflags.split())
    includes = [f"-I{zlib_src}"]
    objs: list[Path] = []
    for name in ZLIB_SRCS:
        src = zlib_src / name
        if not src.exists():
            raise FileNotFoundError(src)
        obj = work / f"{name}.o"
        _compile_one(clang, sysroot, target, src, obj, includes, extra)
        objs.append(obj)
    _archive(llvm_ar, dest, objs)
    shutil.rmtree(work, ignore_errors=True)


def build_compiler_rt_extras(
    clang: Path,
    llvm_ar: Path,
    llvm_src: Path,
    sysroot: Path,
    target: str,
    dest: Path,
    extra_cflags: str = "",
) -> None:
    src = llvm_src / "compiler-rt" / "lib" / "builtins" / "mulodi4.c"
    obj = dest.with_suffix(".o")
    extra = ["-std=c11", "-Wno-unused-parameter"]
    if extra_cflags:
        extra.extend(extra_cflags.split())
    _compile_one(clang, sysroot, target, src, obj, [], extra)
    _archive(llvm_ar, dest, [obj])
    obj.unlink(missing_ok=True)


def _bionic_includes(bionic: Path, support: Path | None, work: Path) -> list[str]:
    inc = [
        f"-I{bionic / 'libc'}",
        f"-I{bionic / 'libc' / 'include'}",
        f"-I{bionic / 'libc' / 'private'}",
        f"-I{bionic / 'libc' / 'bionic'}",
        f"-I{bionic / 'libc' / 'stdio'}",
        f"-I{bionic / 'libc' / 'async_safe' / 'include'}",
        f"-I{bionic / 'libc' / 'platform'}",
        f"-I{bionic / 'libc' / 'system_properties' / 'include'}",
        f"-I{bionic / 'libstdc++' / 'include'}",
        f"-I{work}",
    ]
    if support is not None:
        inc.append(f"-I{support}")
        inc.append(f"-I{support / 'private'}")
        parser = support / "property_info_parser" / "include"
        if parser.is_dir():
            inc.append(f"-I{parser}")
    return inc


def _scudo_srcs(llvm_src: Path) -> list[Path]:
    root = llvm_src / "compiler-rt" / "lib" / "scudo" / "standalone"
    srcs = [root / name for name in SCUDO_SRCS if (root / name).exists()]
    gwp = llvm_src / "compiler-rt" / "lib" / "gwp_asan"
    srcs.extend(sorted(gwp.glob("*.cpp")))
    return [p for p in srcs if p.name not in SCUDO_SKIP]


def _src_arch_ok(src: Path, soong_arch: str) -> bool:
    wanted = f"arch-{soong_arch}"
    for part in src.parts:
        if part.startswith("arch-") and part not in {wanted, "arch-common"}:
            return False
    return True


def _obj_name(work: Path, src: Path, prefix: str) -> Path:
    ident = "__".join(src.with_suffix("").parts[-4:])
    return work / f"{prefix}{ident}.o"


def build_libc(
    clang: Path,
    llvm_ar: Path,
    bionic: Path,
    sysroot: Path,
    abi_name: str,
    api: int,
    dest: Path,
    support: Path | None = None,
    extra_cflags: str = "",
    llvm_src: Path | None = None,
    ucd: Path | None = None,
    arm_opt: Path | None = None,
) -> None:
    info = ABIS[abi_name]
    target = f"{info['clang_triple']}{PLATFORM_API}"
    soong_arch = ARCH_DIR[abi_name]
    bp = bionic / "libc" / "Android.bp"
    work = dest.parent / f".libc-{abi_name}"
    if work.exists():
        shutil.rmtree(work)
    work.mkdir(parents=True)
    if support is not None:
        fs = support / "private" / "android_filesystem_config.h"
        if fs.exists():
            generate_android_ids(fs, work / "generated_android_ids.h")
    syscall_s = gensyscalls(bionic, soong_arch, work / f"syscalls-{soong_arch}.S")
    common_inc = _bionic_includes(bionic, support, work)
    extra = BIONIC_CFLAGS + [f"-DPLATFORM_SDK_VERSION={api}"]
    if extra_cflags:
        extra.extend(extra_cflags.split())
    if llvm_src is not None:
        common_inc.extend(
            [
                f"-I{llvm_src / 'compiler-rt' / 'include'}",
                f"-I{llvm_src / 'compiler-rt' / 'lib'}",
                f"-I{llvm_src / 'compiler-rt' / 'lib' / 'scudo' / 'standalone' / 'include'}",
                f"-I{llvm_src / 'compiler-rt' / 'lib' / 'scudo' / 'standalone'}",
            ]
        )
        extra.extend(["-DUSE_SCUDO", "-DSCUDO_HAS_PLATFORM_TLS_SLOT=1"])

    mods = index_blueprints(bp)
    items: list[tuple[Path, Path, list[str], list[str]]] = []
    seen: set[Path] = set()

    def add(src: Path, extra_flags: list[str], extra_inc: list[str], prefix: str) -> None:
        src = src.resolve()
        if src in seen or not src.is_file():
            return
        if src.suffix not in {".c", ".cc", ".cpp", ".cxx", ".C", ".S", ".s"}:
            return
        if not _src_arch_ok(src, soong_arch):
            return
        seen.add(src)
        items.append((src, _obj_name(work, src, prefix), common_inc + extra_inc, extra + extra_flags))

    add(syscall_s, [], [], "sys_")
    for name in LIBC_MODULES:
        if name not in mods:
            print(f"WARNING: missing Soong module {name}", flush=True)
            continue
        flags = module_cflags(mods, name, soong_arch)
        incs = [f"-I{p}" for p in module_includes(mods, name, soong_arch)]
        srcs = resolve_module_srcs(mods, name, soong_arch)
        print(f"libc module {name}: {len(srcs)} srcs", flush=True)
        for src in srcs:
            add(src, flags, incs, f"{name}_")

    if support is not None:
        parser = support / "property_info_parser" / "property_info_parser.cpp"
        if parser.exists():
            add(parser, ["-std=gnu++20", "-nostdinc++"], [f"-I{support / 'property_info_parser' / 'include'}"], "prop_")

    if ucd is not None:
        ucd_dir = extract_ucd(ucd, work / "ucd")
        icu_c = generate_icu4x_c(ucd_dir, work / "icu4x_bionic.c")
        add(icu_c, ["-std=gnu17"], [], "icu_")

    if arm_opt is not None:
        arm_bp = arm_opt / "Android.bp"
        if arm_bp.exists():
            arm_mods = index_blueprints(arm_bp)
            for name in ARM_OPT_MODULES:
                if name not in arm_mods:
                    continue
                flags = module_cflags(arm_mods, name, soong_arch) + [
                    "-DWANT_ERRNO=0",
                    "-DWANT_VMATH=0",
                    "-DWANT_MOPS=0",
                    "-D__BIONIC_LP32_USE_LONG_DOUBLE",
                ]
                incs = [f"-I{p}" for p in module_includes(arm_mods, name, soong_arch)]
                incs.append(f"-I{arm_opt / 'string'}")
                srcs = resolve_module_srcs(arm_mods, name, soong_arch)
                srcs = [s for s in srcs if "mops" not in s.name]
                print(f"arm-opt module {name}: {len(srcs)} srcs", flush=True)
                for src in srcs:
                    add(src, flags, incs, f"{name}_")

    if llvm_src is not None:
        scudo_flags = extra + ["-DUSE_SCUDO", "-DSCUDO_HAS_PLATFORM_TLS_SLOT=1", "-fno-exceptions"]
        for src in _scudo_srcs(llvm_src):
            add(src, scudo_flags, [], "scudo_")

    print(f"libc.a compiling {len(items)} files", flush=True)
    objs = _compile_many(clang, sysroot, target, items)
    if not objs:
        raise RuntimeError("libc.a: no objects compiled")
    _archive(llvm_ar, dest, objs)
    shutil.rmtree(work, ignore_errors=True)


def build_libm(
    clang: Path,
    llvm_ar: Path,
    bionic: Path,
    sysroot: Path,
    abi_name: str,
    api: int,
    dest: Path,
    extra_cflags: str = "",
    arm_opt: Path | None = None,
) -> None:
    info = ABIS[abi_name]
    target = f"{info['clang_triple']}{PLATFORM_API}"
    soong_arch = ARCH_DIR[abi_name]
    bp = bionic / "libm" / "Android.bp"
    work = dest.parent / f".libm-{abi_name}"
    if work.exists():
        shutil.rmtree(work)
    work.mkdir(parents=True)
    mods = index_blueprints(bp)
    includes = [
        f"-I{bionic / 'libm'}",
        f"-I{bionic / 'libm' / 'include'}",
        f"-I{bionic / 'libc'}",
        f"-I{bionic / 'libc' / 'include'}",
        f"-I{bionic / 'libc' / 'private'}",
    ]
    extra = ["-fno-builtin", "-fPIC", "-O2", "-std=gnu99"]
    extra.extend(module_cflags(mods, "libm", soong_arch))
    if extra_cflags:
        extra.extend(extra_cflags.split())
    extra = _drop_werror(extra)
    for p in module_includes(mods, "libm", soong_arch):
        includes.append(f"-I{p}")
    items: list[tuple[Path, Path, list[str], list[str]]] = []
    seen: set[Path] = set()
    for src in resolve_module_srcs(mods, "libm", soong_arch):
        if src in seen or not src.is_file():
            continue
        if src.suffix not in {".c", ".cc", ".cpp", ".cxx", ".C", ".S", ".s"}:
            continue
        if not _src_arch_ok(src, soong_arch):
            continue
        seen.add(src)
        items.append((src, _obj_name(work, src, "m_"), includes, extra))
    if arm_opt is not None:
        arm_mods = index_blueprints(arm_opt / "Android.bp") if (arm_opt / "Android.bp").exists() else {}
        if "libarm-optimized-routines-math" in arm_mods:
            flags = extra + module_cflags(arm_mods, "libarm-optimized-routines-math", soong_arch)
            incs = includes + [f"-I{p}" for p in module_includes(arm_mods, "libarm-optimized-routines-math", soong_arch)]
            for src in resolve_module_srcs(arm_mods, "libarm-optimized-routines-math", soong_arch):
                if src in seen or not src.is_file():
                    continue
                if src.suffix not in {".c", ".cc", ".cpp", ".cxx", ".C", ".S", ".s"}:
                    continue
                if not _src_arch_ok(src, soong_arch):
                    continue
                seen.add(src)
                items.append((src, _obj_name(work, src, "armm_"), incs, flags))
    print(f"libm.a compiling {len(items)} files", flush=True)
    objs = _compile_many(clang, sysroot, target, items)
    if not objs:
        raise RuntimeError("libm.a: no objects compiled")
    _archive(llvm_ar, dest, objs)
    shutil.rmtree(work, ignore_errors=True)


def install_static_libs(sysroot: Path, abi_name: str, files: dict[str, Path]) -> None:
    info = ABIS[abi_name]
    dest = sysroot / "usr" / "lib" / info["lib_triple"]
    dest.mkdir(parents=True, exist_ok=True)
    for name, src in files.items():
        if src.exists():
            (dest / name).write_bytes(src.read_bytes())


def build_static_for_abi(
    clang: Path,
    llvm_ar: Path,
    bionic: Path,
    sysroot: Path,
    abi_name: str,
    work: Path,
    zlib_src: Path | None = None,
    llvm_src: Path | None = None,
    support: Path | None = None,
    arm_opt: Path | None = None,
    api: int | None = None,
    ucd: Path | None = None,
) -> dict[str, Path]:
    if api is None:
        api = abi_min_api(abi_name)
    info = ABIS[abi_name]
    target = f"{info['clang_triple']}{PLATFORM_API}"
    extra = str(info.get("cflags", ""))
    work.mkdir(parents=True, exist_ok=True)
    built: dict[str, Path] = {}
    dl = work / "libdl.a"
    build_libdl(clang, llvm_ar, bionic, sysroot, target, dl)
    built["libdl.a"] = dl
    stdcxx = work / "libstdc++.a"
    build_libstdcxx(clang, llvm_ar, bionic, sysroot, target, stdcxx)
    built["libstdc++.a"] = stdcxx
    if zlib_src is not None:
        z = work / "libz.a"
        build_libz(clang, llvm_ar, zlib_src, sysroot, target, z, extra_cflags=extra)
        built["libz.a"] = z
    if llvm_src is not None:
        extras = work / "libcompiler_rt-extras.a"
        build_compiler_rt_extras(clang, llvm_ar, llvm_src, sysroot, target, extras, extra_cflags=extra)
        built["libcompiler_rt-extras.a"] = extras
    libc = work / "libc.a"
    build_libc(
        clang,
        llvm_ar,
        bionic,
        sysroot,
        abi_name,
        api,
        libc,
        support=support,
        extra_cflags=extra,
        llvm_src=llvm_src,
        ucd=ucd,
        arm_opt=arm_opt,
    )
    built["libc.a"] = libc
    libm = work / "libm.a"
    build_libm(
        clang, llvm_ar, bionic, sysroot, abi_name, api, libm, extra_cflags=extra, arm_opt=arm_opt
    )
    built["libm.a"] = libm
    return built
