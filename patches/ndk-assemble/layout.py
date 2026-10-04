"""NDK path layout, ABI tables, and host-tag mapping."""

from __future__ import annotations

import shutil
from dataclasses import dataclass
from pathlib import Path

MIN_API = 21

ABIS = {
    "arm64-v8a": {
        "abi": "arm64-v8a",
        "clang_triple": "aarch64-linux-android",
        "lib_triple": "aarch64-linux-android",
        "arch": "aarch64",
    },
    "armeabi-v7a": {
        "abi": "armeabi-v7a",
        "clang_triple": "armv7a-linux-androideabi",
        "lib_triple": "arm-linux-androideabi",
        "arch": "arm",
        "cflags": "-mthumb",
    },
    "x86_64": {
        "abi": "x86_64",
        "clang_triple": "x86_64-linux-android",
        "lib_triple": "x86_64-linux-android",
        "arch": "x86_64",
    },
}

CLANG_TRIPLE_TO_ABI = {info["clang_triple"]: info for info in ABIS.values()}

HOST_TAGS = {
    ("linux", "amd64"): "linux-x86_64",
    ("linux", "arm64"): "linux-aarch64",
    ("darwin", "amd64"): "darwin-x86_64",
    ("darwin", "arm64"): "darwin-arm64",
    ("windows", "amd64"): "windows-x86_64",
}

ZIP_HOST_TAGS = ("linux-x86_64", "darwin-x86_64", "windows-x86_64")


@dataclass(frozen=True)
class Abi:
    abi: str
    clang_triple: str
    lib_triple: str
    arch: str
    cflags: str = ""
    min_api: int = MIN_API

    @classmethod
    def from_clang_triple(cls, triple: str) -> Abi:
        info = CLANG_TRIPLE_TO_ABI.get(triple)
        if info is None:
            raise ValueError(f"unknown clang triple {triple!r}")
        return cls(**{k: v for k, v in info.items() if k in cls.__dataclass_fields__})


def abi_min_api(abi_name: str) -> int:
    return int(ABIS[abi_name].get("min_api", MIN_API))


def clang_has_arch(clang: Path, arch: str) -> bool:
    """True if this clang was built with the backend for `arch`."""
    import subprocess

    try:
        out = subprocess.check_output(
            [str(clang), "-print-targets"], text=True, stderr=subprocess.STDOUT
        )
    except (OSError, subprocess.CalledProcessError) as err:
        print(f"clang -print-targets failed: {err}", flush=True)
        if getattr(err, "output", None):
            print(err.output, end="", flush=True)
        return False
    tokens = {line.strip().split()[0].lower() for line in out.splitlines() if line.strip()}
    wanted = {
        "aarch64": {"aarch64", "arm64"},
        "arm": {"arm", "armeb"},
        "x86_64": {"x86-64", "x86_64"},
    }
    ok = bool(tokens & wanted.get(arch, {arch}))
    if not ok:
        print(f"clang -print-targets missing {arch}:\n{out}", flush=True)
    return ok


def host_tag(goos: str, goarch: str) -> str:
    key = (goos.lower(), goarch.lower())
    try:
        return HOST_TAGS[key]
    except KeyError as exc:
        raise ValueError(f"unsupported builder {goos}_{goarch}") from exc


def toolchain_root(ndk: Path, tag: str) -> Path:
    return ndk / "toolchains" / "llvm" / "prebuilt" / tag


def sysroot_path(ndk: Path, tag: str) -> Path:
    return toolchain_root(ndk, tag) / "sysroot"


def api_levels(sysroot: Path) -> list[int]:
    """API levels present under sysroot/usr/lib/<triple>/<api>."""
    levels: set[int] = set()
    libroot = sysroot / "usr" / "lib"
    if not libroot.is_dir():
        return []
    for triple_dir in libroot.iterdir():
        if not triple_dir.is_dir():
            continue
        for child in triple_dir.iterdir():
            if child.is_dir() and child.name.isdigit():
                levels.add(int(child.name))
    return sorted(levels)


def lib_dir(sysroot: Path, lib_triple: str, api: int | None = None) -> Path:
    path = sysroot / "usr" / "lib" / lib_triple
    if api is not None:
        return path / str(api)
    return path


def prune_sysroot_abis(sysroot: Path, keep: list[str]) -> None:
    """Remove usr/lib/<triple> trees that are not in `keep` (ABI names)."""
    keep_triples = {ABIS[name]["lib_triple"] for name in keep if name in ABIS}
    libroot = sysroot / "usr" / "lib"
    if not libroot.is_dir():
        return
    for child in list(libroot.iterdir()):
        if child.is_dir() and child.name not in keep_triples:
            shutil.rmtree(child)
