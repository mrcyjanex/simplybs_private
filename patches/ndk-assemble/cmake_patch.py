"""Pin ANDROID_HOST_TAG in NDK CMake files to this build's host tag."""

from __future__ import annotations

from pathlib import Path

OVERRIDE = """

# simplybs: pin the host tag to the toolchain directory this NDK was built with.
set(ANDROID_HOST_TAG "{tag}")
"""


def patch_cmake_host_tag(ndk: Path, host_tag: str) -> list[Path]:
    patched: list[Path] = []
    cmake_root = ndk / "build" / "cmake"
    if not cmake_root.is_dir():
        return patched
    needle = "ANDROID_HOST_TAG"
    for path in cmake_root.rglob("*.cmake"):
        text = path.read_text(errors="replace")
        if needle not in text:
            continue
        if f'set(ANDROID_HOST_TAG "{host_tag}")' in text and "simplybs: pin the host tag" in text:
            patched.append(path)
            continue
        path.write_text(text.rstrip() + OVERRIDE.format(tag=host_tag))
        patched.append(path)
    return patched
