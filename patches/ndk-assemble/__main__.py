"""CLI for ndk-assemble."""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
import tempfile
from pathlib import Path

from install import install
from layout import Abi, MIN_API, host_tag, sysroot_path
from runtimes import build_runtimes
from skeleton import prepare_skeleton


def _path(value: str) -> Path:
    return Path(value).resolve()


def _default_host_tag() -> str:
    sysname = os.environ.get("BUILDER_GOOS") or os.uname().sysname.lower()
    machine = os.environ.get("BUILDER_GOARCH")
    if not machine:
        uname_map = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}
        machine = uname_map.get(os.uname().machine, os.uname().machine)
    return host_tag(sysname, machine)


def cmd_prepare_skeleton(args: argparse.Namespace) -> None:
    prepare_skeleton(args.input, args.output, args.host_tag)
    print(args.output)


def cmd_build_runtimes(args: argparse.Namespace) -> None:
    abis = [a.strip() for a in args.abis.split(",") if a.strip()] if args.abis else None
    build_runtimes(
        llvm_src=args.llvm_src,
        clang_prefix=args.clang_prefix,
        sysroot=args.sysroot,
        output=args.output,
        api=args.api,
        abis=abis,
        jobs=args.jobs,
    )
    print(args.output)


def cmd_install(args: argparse.Namespace) -> None:
    runtimes = args.runtimes if args.runtimes else None
    prefix_lib_dir = args.prefix_lib_dir if args.prefix_lib_dir else None
    install(
        skeleton=args.skeleton,
        clang_prefix=args.clang_prefix,
        ndk_out=args.ndk_out,
        host_tag=args.host_tag,
        runtimes=runtimes,
        prefix_lib_dir=prefix_lib_dir,
        target_triple=args.target_triple,
        api=args.api,
    )
    print(args.ndk_out)


def cmd_smoke_test(args: argparse.Namespace) -> None:
    abi = Abi.from_clang_triple(args.target_triple)
    clang = (
        Path(args.ndk)
        / "toolchains"
        / "llvm"
        / "prebuilt"
        / args.host_tag
        / "bin"
        / f"{abi.clang_triple}{args.api}-clang"
    )
    if not clang.exists():
        raise SystemExit(f"missing wrapper {clang}")
    sysroot = sysroot_path(Path(args.ndk), args.host_tag)
    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / "t.c"
        obj = Path(tmp) / "t.o"
        exe = Path(tmp) / "t"
        src.write_text("int main(void) { return 0; }\n")
        subprocess.check_call(
            [str(clang), "-c", str(src), "-o", str(obj), f"--sysroot={sysroot}"]
        )
        subprocess.check_call([str(clang), str(obj), "-o", str(exe), f"--sysroot={sysroot}"])
    print("ok", clang)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="ndk-assemble")
    sub = parser.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("prepare-skeleton", help="strip compiler bits from an NDK zip tree")
    p.add_argument("--input", type=_path, required=True)
    p.add_argument("--output", type=_path, required=True)
    p.add_argument("--host-tag", default=_default_host_tag())
    p.set_defaults(func=cmd_prepare_skeleton)

    p = sub.add_parser("build-runtimes", help="build compiler-rt builtins and libc++")
    p.add_argument("--llvm-src", type=_path, required=True)
    p.add_argument("--clang-prefix", type=_path, required=True)
    p.add_argument("--sysroot", type=_path, required=True)
    p.add_argument("--output", type=_path, required=True)
    p.add_argument("--api", type=int, default=MIN_API)
    p.add_argument("--abis", default="")
    p.add_argument("--jobs", type=int, default=None)
    p.set_defaults(func=cmd_build_runtimes)

    p = sub.add_parser("install", help="assemble the NDK zip layout")
    p.add_argument("--skeleton", type=_path, required=True)
    p.add_argument("--clang-prefix", type=_path, required=True)
    p.add_argument("--runtimes", type=_path, default=None)
    p.add_argument("--ndk-out", type=_path, required=True)
    p.add_argument("--host-tag", default=_default_host_tag())
    p.add_argument("--prefix-lib-dir", type=_path, default=None)
    p.add_argument("--target-triple", default=None)
    p.add_argument("--api", type=int, default=MIN_API)
    p.set_defaults(func=cmd_install)

    p = sub.add_parser("smoke-test", help="compile and link a trivial C program")
    p.add_argument("--ndk", type=_path, required=True)
    p.add_argument("--host-tag", default=_default_host_tag())
    p.add_argument("--target-triple", required=True)
    p.add_argument("--api", type=int, default=MIN_API)
    p.set_defaults(func=cmd_smoke_test)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    args.func(args)
    return 0


if __name__ == "__main__":
    sys.exit(main())
