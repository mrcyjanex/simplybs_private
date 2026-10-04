#!/usr/bin/env python3
"""Tests for ndk-assemble. Run: python3 -m unittest tests.test_ndk_assemble"""

from __future__ import annotations

import os
import shutil
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from cmake_patch import patch_cmake_host_tag  # noqa: E402
from install import install  # noqa: E402
from layout import Abi, api_levels, host_tag, toolchain_root  # noqa: E402
from skeleton import detect_zip_host_tag, is_binary_artifact, prepare_skeleton  # noqa: E402
from wrappers import clang_wrapper  # noqa: E402
from bionic import install_into_sysroot, parse_map_symbols, stub_source  # noqa: E402
from stubs import parse_ast_symbols  # noqa: E402


def _touch(path: Path, text: str = "") -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    if path.suffix in {"", ".sh"} or path.name in {"clang", "clang++"}:
        path.chmod(0o755)


def fake_ndk(root: Path, tag: str = "linux-x86_64") -> Path:
    tc = root / "toolchains" / "llvm" / "prebuilt" / tag
    _touch(tc / "bin" / "clang", "#!/bin/sh\necho prebuilt-clang\n")
    _touch(tc / "bin" / "python3", "#!/bin/sh\necho prebuilt-python\n")
    _touch(tc / "lib" / "clang" / "19" / "include" / "stddef.h", "// old\n")
    _touch(tc / "sysroot" / "usr" / "include" / "stdio.h", "#pragma once\n")
    for triple in ("aarch64-linux-android", "arm-linux-androideabi", "x86_64-linux-android", "i686-linux-android", "riscv64-linux-android"):
        _touch(tc / "sysroot" / "usr" / "lib" / triple / "21" / "libc.so", "stub\n")
        _touch(tc / "sysroot" / "usr" / "lib" / triple / "24" / "libc.so", "stub\n")
        _touch(tc / "sysroot" / "usr" / "lib" / triple / "libc.a", "archive\n")
        _touch(tc / "sysroot" / "usr" / "lib" / triple / "crtbegin_dynamic.o", "obj\n")
    _touch(
        root / "build" / "cmake" / "android.toolchain.cmake",
        'if(CMAKE_HOST_SYSTEM_NAME STREQUAL Linux)\n  set(ANDROID_HOST_TAG linux-x86_64)\nendif()\n',
    )
    _touch(root / "source.properties", "Pkg.Desc = Android NDK\nPkg.Revision = 28.2.13676380\n")
    _touch(root / "meta" / "platforms.json", '{"min":21,"max":35}\n')
    _touch(root / "prebuilt" / tag / "bin" / "make", "#!/bin/sh\necho make\n")
    _touch(root / "shader-tools" / tag / "glslc", "#!/bin/sh\necho glslc\n")
    _touch(root / "ndk-lldb", "#!/bin/sh\n")
    return root


def fake_clang_prefix(root: Path) -> Path:
    _touch(root / "bin" / "clang", "#!/bin/sh\necho clang-from-source\n")
    _touch(root / "bin" / "clang++", "#!/bin/sh\necho clang++-from-source\n")
    _touch(root / "bin" / "ld.lld", "#!/bin/sh\necho lld\n")
    _touch(root / "bin" / "llvm-ar", "#!/bin/sh\necho ar\n")
    _touch(root / "bin" / "llvm-as", "#!/bin/sh\necho as\n")
    _touch(root / "bin" / "llvm-ranlib", "#!/bin/sh\necho ranlib\n")
    _touch(root / "bin" / "llvm-nm", "#!/bin/sh\necho nm\n")
    _touch(root / "bin" / "llvm-strip", "#!/bin/sh\necho strip\n")
    _touch(root / "lib" / "clang" / "21" / "include" / "stddef.h", "// from source\n")
    return root


def fake_runtimes(root: Path) -> Path:
    lib = root / "arm64-v8a" / "lib"
    _touch(lib / "libclang_rt.builtins-aarch64-android.a", "builtins\n")
    _touch(lib / "libc++_shared.so", "c++\n")
    _touch(lib / "libc++_static.a", "c++static\n")
    _touch(lib / "libc++abi.a", "abi\n")
    _touch(root / "arm64-v8a" / "include" / "c++" / "v1" / "string", "// libc++\n")
    return root


class HostTagTests(unittest.TestCase):
    def test_linux_amd64(self) -> None:
        self.assertEqual(host_tag("linux", "amd64"), "linux-x86_64")

    def test_linux_arm64(self) -> None:
        self.assertEqual(host_tag("linux", "arm64"), "linux-aarch64")

    def test_darwin_arm64(self) -> None:
        self.assertEqual(host_tag("darwin", "arm64"), "darwin-arm64")

    def test_unknown(self) -> None:
        with self.assertRaises(ValueError):
            host_tag("plan9", "arm")

    def test_armv7_cflags(self) -> None:
        abi = Abi.from_clang_triple("armv7a-linux-androideabi")
        self.assertEqual(abi.cflags, "-mthumb")
        self.assertEqual(abi.lib_triple, "arm-linux-androideabi")

    def test_unknown_triple_rejected(self) -> None:
        with self.assertRaises(ValueError):
            Abi.from_clang_triple("riscv64-linux-android")


class WrapperTests(unittest.TestCase):
    def test_cc1_passthrough(self) -> None:
        text = clang_wrapper("clang", "aarch64-linux-android21")
        self.assertIn('--target=aarch64-linux-android21', text)
        self.assertIn('"$1" != "-cc1"', text)
        self.assertIn('"$bin_dir/clang"', text)
        self.assertTrue(text.startswith("#!/usr/bin/env bash\n"))


class SkeletonTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def test_prepare_strips_compiler_and_zip_binaries(self) -> None:
        src = fake_ndk(self.tmp / "zip")
        self.assertEqual(detect_zip_host_tag(src), "linux-x86_64")
        out = prepare_skeleton(src, self.tmp / "skel", "linux-aarch64")
        tc = toolchain_root(out, "linux-aarch64")
        self.assertTrue((tc / "sysroot" / "usr" / "include" / "stdio.h").exists())
        self.assertFalse((tc / "bin").exists())
        self.assertFalse((tc / "lib").exists())
        self.assertFalse(toolchain_root(out, "linux-x86_64").exists())
        self.assertTrue((out / "source.properties").exists())
        self.assertTrue((out / "build" / "cmake" / "android.toolchain.cmake").exists())
        self.assertFalse((out / "prebuilt").exists())
        self.assertFalse((out / "shader-tools").exists())
        self.assertFalse((out / "ndk-lldb").exists())
        lib = tc / "sysroot" / "usr" / "lib" / "aarch64-linux-android"
        self.assertFalse((lib / "21" / "libc.so").exists())
        self.assertFalse((lib / "crtbegin_dynamic.o").exists())
        self.assertFalse((lib / "libc.a").exists())
        self.assertEqual(api_levels(tc / "sysroot"), [21, 24])
        self.assertFalse((tc / "sysroot" / "usr" / "lib" / "i686-linux-android").exists())
        self.assertFalse((tc / "sysroot" / "usr" / "lib" / "riscv64-linux-android").exists())


class InstallTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def test_install_zip_layout_and_compat_bin(self) -> None:
        src = fake_ndk(self.tmp / "zip")
        skel = prepare_skeleton(src, self.tmp / "skel", "linux-x86_64")
        lib21 = (
            skel
            / "toolchains"
            / "llvm"
            / "prebuilt"
            / "linux-x86_64"
            / "sysroot"
            / "usr"
            / "lib"
            / "aarch64-linux-android"
            / "21"
        )
        _touch(lib21 / "libc.so", "from-source-stub\n")
        _touch(lib21 / "crtbegin_dynamic.o", "from-source-crt\n")
        clang = fake_clang_prefix(self.tmp / "clang")
        runtimes = fake_runtimes(self.tmp / "rt")
        ndk = self.tmp / "ndk"
        prefix_lib = self.tmp / "prefix" / "lib"
        host_lib = self.tmp / "hostlib"
        _touch(host_lib / "libc++abi.so.1", "abi")
        _touch(host_lib / "libunwind.so.1", "unwind")
        install(
            skeleton=skel,
            clang_prefix=clang,
            ndk_out=ndk,
            host_tag="linux-x86_64",
            runtimes=runtimes,
            prefix_lib_dir=prefix_lib,
            host_lib_dir=host_lib,
            target_triple="aarch64-linux-android",
            api=21,
        )
        tc = toolchain_root(ndk, "linux-x86_64")
        self.assertTrue((tc / "bin" / "clang").exists())
        self.assertTrue((tc / "bin" / "llvm-as").exists())
        self.assertTrue((tc / "bin" / "as").is_symlink())
        wrapper = tc / "bin" / "aarch64-linux-android21-clang"
        self.assertTrue(wrapper.exists())
        text = wrapper.read_text()
        self.assertIn("--target=aarch64-linux-android21", text)
        self.assertTrue((tc / "bin" / "aarch64-linux-android24-clang").exists())
        self.assertFalse((tc / "bin" / "armv7a-linux-androideabi21-clang").exists())
        self.assertTrue((tc / "bin" / "aarch64-linux-android-ld").exists())
        self.assertTrue((tc / "lib" / "clang" / "21" / "include" / "stddef.h").exists())
        builtins = tc / "lib" / "clang" / "21" / "lib" / "linux" / "libclang_rt.builtins-aarch64-android.a"
        self.assertTrue(builtins.exists())
        self.assertTrue((tc / "sysroot" / "usr" / "lib" / "aarch64-linux-android" / "libc++_shared.so").exists())
        self.assertTrue((tc / "sysroot" / "usr" / "include" / "c++" / "v1" / "string").exists())
        cmake = (ndk / "build" / "cmake" / "android.toolchain.cmake").read_text()
        self.assertIn('set(ANDROID_HOST_TAG "linux-x86_64")', cmake)
        self.assertIn("simplybs: pin the host tag", cmake)
        compat = ndk / "bin" / "aarch64-linux-android21-clang"
        self.assertTrue(compat.exists())
        self.assertIn("toolchains/llvm/prebuilt", compat.read_text())
        self.assertTrue((prefix_lib / "libc.so").exists())
        self.assertTrue((prefix_lib / "crtbegin_dynamic.o").exists())
        self.assertFalse((prefix_lib / "libc.a").exists())
        self.assertTrue((tc / "lib" / "libc++abi.so.1").exists())
        self.assertTrue((tc / "lib" / "libunwind.so.1").exists())
        self.assertEqual(api_levels(tc / "sysroot"), [21, 24])

    def test_patch_idempotent(self) -> None:
        ndk = fake_ndk(self.tmp / "ndk")
        patch_cmake_host_tag(ndk, "linux-aarch64")
        patch_cmake_host_tag(ndk, "linux-aarch64")
        text = (ndk / "build" / "cmake" / "android.toolchain.cmake").read_text()
        self.assertEqual(text.count("simplybs: pin the host tag"), 1)


class MapTests(unittest.TestCase):
    def test_filters_arch_and_api(self) -> None:
        text = """
LIBC {
  global:
    malloc;
    foo; # introduced=24
    bar; # arm
    baz; # arm64 introduced=21
    stdin; # var
    hidden; # apex
  local:
    *;
};
"""
        arm64 = parse_map_symbols(text, "arm64", 21)
        names = [n for n, _, _ in arm64]
        self.assertIn("malloc", names)
        self.assertIn("baz", names)
        self.assertIn("stdin", names)
        self.assertNotIn("foo", names)
        self.assertNotIn("bar", names)
        self.assertNotIn("hidden", names)
        kinds = {n: k for n, k, _ in arm64}
        self.assertEqual(kinds["stdin"], "obj")
        src = stub_source(arm64)
        self.assertIn('__asm__("malloc")', src)
        from bionic import version_script

        vs = version_script("libc.so", arm64, text)
        self.assertTrue(vs.startswith("LIBC {"))
        self.assertNotIn("LIBC_SO", vs)

    def test_header_ast_filters_path_and_inline(self) -> None:
        dump = """
|-FunctionDecl 0x1 </tmp/sysroot/usr/include/stdlib.h:1:1, col:8> col:5 malloc 'void *(size_t)'
|-FunctionDecl 0x2 </tmp/sysroot/usr/include/android/log.h:102:1, col:68> col:5 __android_log_write 'int (int, const char *, const char *)'
|-FunctionDecl 0x3 <line:41:1, col:42> col:42 android_get_device_api_level 'int ()' static inline
|-VarDecl 0x4 </tmp/sysroot/usr/include/android/log.h:50:1> col:12 used android_log_id 'int' extern
"""
        symbols = parse_ast_symbols(dump, ("android/log.h",))
        names = [n for n, _, _ in symbols]
        self.assertIn("__android_log_write", names)
        self.assertNotIn("malloc", names)
        self.assertNotIn("android_get_device_api_level", names)
        kinds = {n: k for n, k, _ in symbols}
        self.assertEqual(kinds["android_log_id"], "obj")

    def test_crt_and_stubs_only_in_api_dir(self) -> None:
        d = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, d, True)
        crt = d / "crt"
        stub = d / "stub"
        sysroot = d / "sysroot"
        crt.mkdir()
        stub.mkdir()
        (crt / "crtbegin_dynamic.o").write_bytes(b"crt")
        (stub / "libc.so").write_bytes(b"stub")
        install_into_sysroot(sysroot, "arm64-v8a", 21, crt, stub)
        lib = sysroot / "usr" / "lib" / "aarch64-linux-android"
        self.assertTrue((lib / "21" / "crtbegin_dynamic.o").exists())
        self.assertTrue((lib / "21" / "libc.so").exists())
        self.assertFalse((lib / "crtbegin_dynamic.o").exists())
        self.assertFalse((lib / "libc.so").exists())
        self.assertIn("INPUT(-lc++_shared)", (lib / "21" / "libc++.so").read_text())

    def test_bp_srcs_from_snippet(self) -> None:
        from bp import module_srcs

        bp = """
cc_library_static {
    srcs: ["a.c", "b.cpp"],
    arch: { arm64: { srcs: ["c.S"] } },
    name: "foo",
}
"""
        self.assertEqual(module_srcs(bp, "foo"), ["a.c", "b.cpp"])
        self.assertEqual(module_srcs(bp, "foo", "arm64"), ["a.c", "b.cpp", "c.S"])

    def test_bp_glob_filegroup_defaults_and_nested_list(self) -> None:
        from bp import index_blueprints, resolve_module_srcs, module_cflags

        d = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, d, True)
        (d / "tzcode").mkdir()
        (d / "tzcode" / "a.c").write_text("int a;")
        (d / "tzcode" / "b.c").write_text("int b;")
        (d / "note.cpp").write_text("int n;")
        (d / "Android.bp").write_text(
            """
filegroup {
    name: "elf_note_sources",
    srcs: ["note.cpp"],
}
cc_defaults {
    name: "base",
    cflags: ["-DFOO"],
    arch: { arm64: { srcs: ["arch.c"] } },
}
cc_library_static {
    name: "libc_tzcode",
    defaults: ["base"],
    srcs: [
        "tzcode/**/*.c",
        ":elf_note_sources",
    ],
    cflags: ["-DALL_STATE"],
}
"""
        )
        (d / "arch.c").write_text("int arch;")
        mods = index_blueprints(d / "Android.bp")
        srcs = {p.name for p in resolve_module_srcs(mods, "libc_tzcode", "arm64")}
        self.assertEqual(srcs, {"a.c", "b.c", "note.cpp", "arch.c"})
        flags = module_cflags(mods, "libc_tzcode", "arm64")
        self.assertIn("-DFOO", flags)
        self.assertIn("-DALL_STATE", flags)

    def test_icu4x_tables_from_tiny_ucd(self) -> None:
        from unicode import generate_icu4x_c

        d = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, d, True)
        (d / "UnicodeData.txt").write_text(
            "0041;LATIN CAPITAL LETTER A;Lu;0;L;;;;;N;;;;0061;\n"
            "0061;LATIN SMALL LETTER A;Ll;0;L;;;;;N;;;0041;;0041\n"
            "0030;DIGIT ZERO;Nd;0;EN;;0;0;0;N;;;;;\n"
        )
        (d / "DerivedCoreProperties.txt").write_text(
            "0041..005A ; Alphabetic # Lu\n0061..007A ; Alphabetic # Ll\n"
            "0061..007A ; Lowercase # Ll\n0041..005A ; Uppercase # Lu\n"
        )
        (d / "PropList.txt").write_text("0009..000D ; White_Space # Cc\n0030..0039 ; Hex_Digit # Nd\n")
        (d / "EastAsianWidth.txt").write_text("4E00..9FFF ; W # Lo\n")
        (d / "HangulSyllableType.txt").write_text("AC00 ; LV # Lo\n")
        out = d / "icu4x.c"
        generate_icu4x_c(d, out)
        text = out.read_text()
        self.assertIn("__icu4x_bionic_general_category", text)
        self.assertIn("0x41", text)
        self.assertIn("kEastAsianWidth", text)


    def test_zip_elf_is_binary_text_script_is_not(self) -> None:
        d = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, d, True)
        elf = d / "libc.so"
        script = d / "libc++.so"
        elf.write_bytes(b"\x7fELF" + b"\0" * 16)
        script.write_text("INPUT(-lc++_shared)\n")
        self.assertTrue(is_binary_artifact(elf))
        self.assertFalse(is_binary_artifact(script))


if __name__ == "__main__":
    unittest.main()
