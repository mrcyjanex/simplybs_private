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
from layout import api_levels, host_tag, toolchain_root  # noqa: E402
from skeleton import detect_zip_host_tag, prepare_skeleton  # noqa: E402
from wrappers import clang_wrapper  # noqa: E402


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
    for triple in ("aarch64-linux-android", "arm-linux-androideabi", "x86_64-linux-android"):
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
    return root


def fake_clang_prefix(root: Path) -> Path:
    _touch(root / "bin" / "clang", "#!/bin/sh\necho clang-from-source\n")
    _touch(root / "bin" / "clang++", "#!/bin/sh\necho clang++-from-source\n")
    _touch(root / "bin" / "ld.lld", "#!/bin/sh\necho lld\n")
    _touch(root / "bin" / "llvm-ar", "#!/bin/sh\necho ar\n")
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


class WrapperTests(unittest.TestCase):
    def test_cc1_passthrough(self) -> None:
        text = clang_wrapper("clang", "aarch64-linux-android21")
        self.assertIn('--target=aarch64-linux-android21', text)
        self.assertIn('"$1" != "-cc1"', text)
        self.assertIn('"$bin_dir/clang"', text)


class SkeletonTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def test_prepare_strips_compiler_and_renames_host(self) -> None:
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


class InstallTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)

    def test_install_zip_layout_and_compat_bin(self) -> None:
        src = fake_ndk(self.tmp / "zip")
        skel = prepare_skeleton(src, self.tmp / "skel", "linux-x86_64")
        clang = fake_clang_prefix(self.tmp / "clang")
        runtimes = fake_runtimes(self.tmp / "rt")
        ndk = self.tmp / "ndk"
        prefix_lib = self.tmp / "prefix" / "lib"
        install(
            skeleton=skel,
            clang_prefix=clang,
            ndk_out=ndk,
            host_tag="linux-x86_64",
            runtimes=runtimes,
            prefix_lib_dir=prefix_lib,
            target_triple="aarch64-linux-android",
            api=21,
        )
        tc = toolchain_root(ndk, "linux-x86_64")
        self.assertTrue((tc / "bin" / "clang").exists())
        wrapper = tc / "bin" / "aarch64-linux-android21-clang"
        self.assertTrue(wrapper.exists())
        text = wrapper.read_text()
        self.assertIn("--target=aarch64-linux-android21", text)
        self.assertTrue((tc / "bin" / "aarch64-linux-android24-clang").exists())
        self.assertTrue((tc / "bin" / "armv7a-linux-androideabi21-clang").exists())
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
        self.assertTrue((prefix_lib / "libc.a").exists())
        self.assertTrue((prefix_lib / "crtbegin_dynamic.o").exists())
        self.assertEqual(api_levels(tc / "sysroot"), [21, 24])

    def test_patch_idempotent(self) -> None:
        ndk = fake_ndk(self.tmp / "ndk")
        patch_cmake_host_tag(ndk, "linux-aarch64")
        patch_cmake_host_tag(ndk, "linux-aarch64")
        text = (ndk / "build" / "cmake" / "android.toolchain.cmake").read_text()
        self.assertEqual(text.count("simplybs: pin the host tag"), 1)


if __name__ == "__main__":
    unittest.main()
