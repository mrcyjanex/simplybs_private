"""Build Android CRT objects and NDK stub .so files from bionic source."""

from __future__ import annotations

import json
import shutil
import subprocess
import tempfile
from pathlib import Path

from layout import ABIS, MIN_API, abi_min_api, clang_has_arch, prune_sysroot_abis
from static import build_static_for_abi, install_static_libs
from stubs import STUB_LIBS, extract_header_symbols

MAP_ARCH = {
    "arm64-v8a": "arm64",
    "armeabi-v7a": "arm",
    "x86_64": "x86_64",
    "x86": "x86",
    "riscv64": "riscv64",
}

ARCH_TAGS = {"arm", "arm64", "x86", "x86_64", "riscv64"}
SKIP_TAGS = {"apex", "platform-only", "future"}
# Not part of the NDK stub ABI. LIBC_PRIVATE repeats public ARM EABI names
# (__aeabi_atexit and the rest) and also lists compiler-rt helpers.
SKIP_NODES = {"LIBC_PRIVATE", "LIBC_PLATFORM", "LIBC_DEPRECATED"}

BIONIC_MAPS = {
    "libc.so": "libc/libc.map.txt",
    "libm.so": "libm/libm.map.txt",
    "libdl.so": "libdl/libdl.map.txt",
    "libstdc++.so": "libc/libstdc++.map.txt",
}

CRT_OBJECTS = (
    "crtbegin_dynamic.o",
    "crtbegin_so.o",
    "crtbegin_static.o",
    "crtend_android.o",
    "crtend_so.o",
    "crt_pad_segment.o",
)


def _run(cmd: list[str], cwd: Path | None = None) -> None:
    print("+", " ".join(cmd), flush=True)
    subprocess.check_call(cmd, cwd=cwd)


def parse_map_symbols(text: str, arch: str, api: int) -> list[tuple[str, str, bool]]:
    """Return (name, kind, weak) for symbols visible on this arch/API."""
    out: list[tuple[str, str, bool]] = []
    seen: set[str] = set()
    in_global = False
    node = ""
    for raw in text.splitlines():
        line, _, comment = raw.partition("#")
        line = line.strip()
        comment = comment.strip()
        if not line:
            continue
        if line.endswith("{"):
            node = line[:-1].strip().split()[0]
            in_global = False
            continue
        if line.startswith("global:"):
            in_global = True
            continue
        if line.startswith("local:"):
            in_global = False
            continue
        if line.startswith("}"):
            in_global = False
            continue
        if not in_global or node in SKIP_NODES:
            continue
        name = line.rstrip(";").strip()
        if not name or name == "*":
            continue
        tags = comment.replace(",", " ").split()
        arches: set[str] = set()
        introduced = 0
        introduced_arch: dict[str, int] = {}
        kind = "func"
        weak = False
        skip = False
        for tok in tags:
            if tok in ARCH_TAGS:
                arches.add(tok)
            elif tok == "var":
                kind = "obj"
            elif tok == "weak":
                weak = True
            elif tok in SKIP_TAGS:
                skip = True
            elif tok.startswith("introduced="):
                introduced = int(tok.split("=", 1)[1])
            elif tok.startswith("introduced-") and "=" in tok:
                key, val = tok.split("=", 1)
                a = key[len("introduced-") :]
                if a in {"x64_64", "x86-64"}:
                    a = "x86_64"
                if a in ARCH_TAGS:
                    introduced_arch[a] = int(val)
        if skip:
            continue
        if arches and arch not in arches:
            continue
        if introduced and api < introduced:
            continue
        if arch in introduced_arch and api < introduced_arch[arch]:
            continue
        if name in seen:
            continue
        seen.add(name)
        out.append((name, kind, weak))
    return out


def stub_source(symbols: list[tuple[str, str, bool]]) -> str:
    lines = ["/* generated NDK stub — empty definitions for the public ABI */", ""]
    for i, (name, kind, weak) in enumerate(symbols):
        w = "__attribute__((weak)) " if weak else ""
        ident = f"__ndk_stub_{i}"
        if kind == "obj":
            lines.append(f'{w}char {ident}[sizeof(void *)];')
            lines.append(f'__asm__(".globl {name}; .set {name}, {ident}");')
        else:
            lines.append(f'{w}void {ident}(void) __asm__("{name}");')
            lines.append(f"{w}void {ident}(void) {{}}")
    lines.append("")
    return "\n".join(lines)


def version_tag(soname: str, map_text: str | None = None) -> str:
    """ELF version node: first `{` name in a bionic map, else SONAME without .so."""
    if map_text:
        for raw in map_text.splitlines():
            line, _, _ = raw.partition("#")
            line = line.strip()
            if line.endswith("{"):
                tag = line[:-1].strip()
                if tag:
                    return tag
    return soname.removesuffix(".so").upper().replace("-", "_").replace("++", "XX")


def version_script(
    soname: str,
    symbols: list[tuple[str, str, bool]],
    map_text: str | None = None,
) -> str:
    names = "\n".join(f"    {name};" for name, _, _ in symbols)
    return f"{version_tag(soname, map_text)} {{\n  global:\n{names}\n  local:\n    *;\n}};\n"


def _clang_common(clang: Path, sysroot: Path, target: str, api: int) -> list[str]:
    args = [
        str(clang),
        f"--target={target}",
        f"--sysroot={sysroot}",
        "-fuse-ld=lld",
        f"-DPLATFORM_SDK_VERSION={api}",
        '-DABI_NDK_VERSION="r28c"',
        '-DABI_NDK_BUILD_NUMBER="0"',
        "-O2",
        "-fPIC",
        "-nostdlib",
        "-Wa,--noexecstack",
        "-Wl,-z,noexecstack",
    ]
    if "aarch64" in target:
        args.append("-mbranch-protection=standard")
    if "armv7" in target:
        args.append("-mthumb")
    return args


def build_crt(
    clang: Path,
    bionic: Path,
    crt_src: Path,
    sysroot: Path,
    abi_name: str,
    api: int,
    dest: Path,
) -> None:
    info = ABIS[abi_name]
    target = f"{info['clang_triple']}{api}"
    libc = bionic / "libc"
    common = _clang_common(clang, sysroot, target, api) + [
        f"-I{libc}",
        f"-I{libc / 'arch-common' / 'bionic'}",
        "-Wl,-r",
        "-no-pie",
    ]
    dest.mkdir(parents=True, exist_ok=True)
    brand = crt_src / "crtbrand.S"
    begin = libc / "arch-common" / "bionic" / "crtbegin.c"
    begin_so = libc / "arch-common" / "bionic" / "crtbegin_so.c"
    jobs = [
        ("crtbegin_dynamic.o", [begin, brand], []),
        ("crtbegin_so.o", [begin_so, brand], []),
        ("crtbegin_static.o", [begin, brand], ["-DCRTBEGIN_STATIC", "-D_FORCE_CRT_ATFORK"]),
        ("crtend_android.o", [crt_src / "crtend.S"], []),
        ("crtend_so.o", [crt_src / "crtend_so.S"], []),
        ("crt_pad_segment.o", [crt_src / "crt_pad_segment.S"], []),
    ]
    for name, srcs, extra in jobs:
        _run(common + extra + ["-o", str(dest / name)] + [str(s) for s in srcs])


def _link_stub(
    clang: Path,
    sysroot: Path,
    target: str,
    api: int,
    soname: str,
    src: Path,
    script: Path,
    dest: Path,
    crt_dir: Path | None = None,
) -> None:
    cmd = _clang_common(clang, sysroot, target, api) + [
        "-shared",
        f"-Wl,-soname,{soname}",
        f"-Wl,--version-script,{script}",
        "-o",
        str(dest),
        str(src),
    ]
    if crt_dir is not None and soname != "libc.so":
        # libc.so already exports pthread_atfork; crtbegin_so.o does too (API 23+).
        cmd.extend([str(crt_dir / "crtbegin_so.o"), str(crt_dir / "crtend_so.o")])
    _run(cmd)


def build_stubs(
    clang: Path,
    bionic: Path,
    sysroot: Path,
    abi_name: str,
    api: int,
    crt_dir: Path,
    dest: Path,
    system_libs: dict[str, int] | None = None,
) -> None:
    info = ABIS[abi_name]
    arch = MAP_ARCH[abi_name]
    target = f"{info['clang_triple']}{api}"
    dest.mkdir(parents=True, exist_ok=True)
    work = dest / ".stub-src"
    work.mkdir(parents=True, exist_ok=True)
    built: set[str] = set()
    for soname, rel in BIONIC_MAPS.items():
        map_text = (bionic / rel).read_text()
        symbols = parse_map_symbols(map_text, arch, api)
        src = work / f"{soname}.c"
        script = work / f"{soname}.map"
        src.write_text(stub_source(symbols))
        script.write_text(version_script(soname, symbols, map_text))
        _link_stub(
            clang, sysroot, target, api, soname, src, script, dest / soname, crt_dir=crt_dir
        )
        built.add(soname)
    if system_libs:
        empty_src = work / "empty.c"
        empty_src.write_text("/* empty NDK stub */\n")
        empty_map = work / "empty.map"
        empty_map.write_text("{\n  global:\n  local:\n    *;\n};\n")
        for soname, min_api in system_libs.items():
            if soname in built or api < int(min_api):
                continue
            spec = STUB_LIBS.get(soname)
            symbols: list[tuple[str, str, bool]] = []
            if spec is not None:
                symbols = extract_header_symbols(clang, sysroot, target, api, spec)
            if symbols:
                src = work / f"{soname}.c"
                script = work / f"{soname}.map"
                src.write_text(stub_source(symbols))
                script.write_text(version_script(soname, symbols))
                _link_stub(
                    clang, sysroot, target, api, soname, src, script, dest / soname, crt_dir=crt_dir
                )
            else:
                _link_stub(
                    clang,
                    sysroot,
                    target,
                    api,
                    soname,
                    empty_src,
                    empty_map,
                    dest / soname,
                )
            built.add(soname)


def install_into_sysroot(sysroot: Path, abi_name: str, api: int, crt_dir: Path, stub_dir: Path) -> None:
    info = ABIS[abi_name]
    lib = sysroot / "usr" / "lib" / info["lib_triple"]
    api_dir = lib / str(api)
    api_dir.mkdir(parents=True, exist_ok=True)
    for name in CRT_OBJECTS:
        src = crt_dir / name
        if src.exists():
            (api_dir / name).write_bytes(src.read_bytes())
    for src in stub_dir.glob("*.so"):
        (api_dir / src.name).write_bytes(src.read_bytes())
    # Zip layout: libc++ linker scripts live in every API directory.
    cxx_so = lib / "libc++.so"
    cxx_a = lib / "libc++.a"
    if not cxx_so.exists():
        cxx_so.write_text("INPUT(-lc++_shared)\n")
    if not cxx_a.exists():
        cxx_a.write_text("INPUT(-lc++_static -lc++abi)\n")
    (api_dir / "libc++.so").write_text(cxx_so.read_text())
    (api_dir / "libc++.a").write_text(cxx_a.read_text())


def load_system_libs(ndk: Path) -> dict[str, int]:
    path = ndk / "meta" / "system_libs.json"
    if not path.exists():
        return {}
    raw = json.loads(path.read_text())
    return {str(k): int(v) for k, v in raw.items()}


def load_api_range(ndk: Path) -> tuple[int, int]:
    path = ndk / "meta" / "platforms.json"
    if not path.exists():
        return MIN_API, MIN_API
    raw = json.loads(path.read_text())
    return int(raw.get("min", MIN_API)), int(raw.get("max", MIN_API))


def build_sysroot(
    bionic: Path,
    clang_prefix: Path,
    sysroot: Path,
    crt_src: Path,
    ndk_meta: Path,
    abis: list[str] | None = None,
    apis: list[int] | None = None,
    zlib_src: Path | None = None,
    llvm_src: Path | None = None,
    support: Path | None = None,
    arm_opt: Path | None = None,
    ucd: Path | None = None,
) -> None:
    clang = clang_prefix / "bin" / "clang"
    llvm_ar = clang_prefix / "bin" / "llvm-ar"
    if not clang.exists():
        raise FileNotFoundError(clang)
    requested = list(ABIS) if abis is None else list(abis)
    implicit = abis is None
    prune_sysroot_abis(sysroot, requested)
    if apis is None:
        lo, hi = load_api_range(ndk_meta)
        apis = list(range(lo, hi + 1))
    system_libs = load_system_libs(ndk_meta)
    work = Path(tempfile.mkdtemp(prefix="android-bionic-"))
    try:
        for abi_name in requested:
            info = ABIS[abi_name]
            if not clang_has_arch(clang, info["arch"]):
                msg = f"clang has no backend for {info['arch']} ({abi_name})"
                if not implicit:
                    raise RuntimeError(msg + "; rebuild native/android-clang with that LLVM target")
                print("WARNING:", msg, "- skipped until android-clang includes that target", flush=True)
                continue
            min_api = abi_min_api(abi_name)
            abi_apis = [a for a in apis if a >= min_api]
            if not abi_apis:
                continue
            for api in abi_apis:
                crt_dir = work / f"crt-{abi_name}-{api}"
                stub_dir = work / f"stub-{abi_name}-{api}"
                build_crt(clang, bionic, crt_src, sysroot, abi_name, api, crt_dir)
                build_stubs(
                    clang,
                    bionic,
                    sysroot,
                    abi_name,
                    api,
                    crt_dir,
                    stub_dir,
                    system_libs=system_libs,
                )
                install_into_sysroot(sysroot, abi_name, api, crt_dir, stub_dir)
            static_dir = work / f"static-{abi_name}"
            built = build_static_for_abi(
                clang,
                llvm_ar,
                bionic,
                sysroot,
                abi_name,
                static_dir,
                zlib_src=zlib_src,
                llvm_src=llvm_src,
                support=support,
                arm_opt=arm_opt,
                api=min_api,
                ucd=ucd,
            )
            install_static_libs(sysroot, abi_name, built)
    finally:
        shutil.rmtree(work, ignore_errors=True)
