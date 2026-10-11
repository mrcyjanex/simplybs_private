"""Generate NDK stub .so files from public headers (clang AST) and bionic maps."""

from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass
from pathlib import Path

FUNC_RE = re.compile(
    r"FunctionDecl 0x[0-9a-f]+ <[^>]+>\s+"
    r"(?:(?:line|col):\d+(?::\d+)?\s+)*"
    r"(?:used\s+)?(?:implicit\s+)?(?:invalid\s+)?"
    r"([A-Za-z_][A-Za-z0-9_]*)\s+'([^']*)'(.*)$"
)
VAR_RE = re.compile(
    r"VarDecl 0x[0-9a-f]+ <[^>]+>\s+"
    r"(?:(?:line|col):\d+(?::\d+)?\s+)*"
    r"(?:used\s+)?(?:implicit\s+)?"
    r"([A-Za-z_][A-Za-z0-9_]*)\s+'([^']*)'(.*)$"
)
PATH_RE = re.compile(r"<(/[^:>]+):\d+")


@dataclass(frozen=True)
class StubSpec:
    headers: tuple[str, ...]
    allow: tuple[str, ...]
    lang: str = "c"
    defines: tuple[str, ...] = ()


# soname -> headers whose declarations belong in that stub.
STUB_LIBS: dict[str, StubSpec] = {
    "liblog.so": StubSpec(
        headers=("android/log.h", "android/set_abort_message.h", "android/trace.h"),
        allow=("android/log.h", "android/set_abort_message.h", "android/trace.h"),
    ),
    "libandroid.so": StubSpec(
        headers=(
            "android/native_activity.h",
            "android/looper.h",
            "android/configuration.h",
            "android/input.h",
            "android/sensor.h",
            "android/storage_manager.h",
            "android/obb.h",
            "android/asset_manager.h",
            "android/asset_manager_jni.h",
            "android/choreographer.h",
            "android/multinetwork.h",
            "android/sharedmem.h",
            "android/sharedmem_jni.h",
            "android/font.h",
            "android/font_matcher.h",
            "android/system_fonts.h",
            "android/thermal.h",
            "android/performance_hint.h",
            "android/permission_manager.h",
            "android/imagedecoder.h",
            "android/file_descriptor_jni.h",
        ),
        allow=("android/",),
    ),
    "libjnigraphics.so": StubSpec(
        headers=("android/bitmap.h",),
        allow=("android/bitmap.h",),
    ),
    "libnativewindow.so": StubSpec(
        headers=(
            "android/native_window.h",
            "android/native_window_jni.h",
            "android/hardware_buffer.h",
            "android/hardware_buffer_jni.h",
            "android/hardware_buffer_aidl.h",
            "android/window.h",
            "android/surface_control.h",
            "android/surface_texture.h",
            "android/surface_texture_jni.h",
            "android/data_space.h",
        ),
        allow=("android/native_window", "android/hardware_buffer", "android/window.h",
               "android/surface_", "android/data_space", "android/hdr_metadata", "android/rect.h"),
    ),
    "libsync.so": StubSpec(headers=("android/sync.h",), allow=("android/sync.h",)),
    "libbinder_ndk.so": StubSpec(
        headers=(
            "android/binder_ibinder.h",
            "android/binder_ibinder_jni.h",
            "android/binder_parcel.h",
            "android/binder_parcel_jni.h",
            "android/binder_status.h",
        ),
        allow=("android/binder_",),
    ),
    "libneuralnetworks.so": StubSpec(
        headers=("android/NeuralNetworks.h",),
        allow=("android/NeuralNetworks",),
    ),
    "libEGL.so": StubSpec(
        headers=("EGL/egl.h", "EGL/eglext.h"),
        allow=("EGL/",),
        defines=("EGL_EGLEXT_PROTOTYPES=1",),
    ),
    "libGLESv1_CM.so": StubSpec(
        headers=("GLES/gl.h", "GLES/glext.h"),
        allow=("GLES/",),
        defines=("GL_GLEXT_PROTOTYPES=1",),
    ),
    "libGLESv2.so": StubSpec(
        headers=("GLES2/gl2.h", "GLES2/gl2ext.h"),
        allow=("GLES2/",),
        defines=("GL_GLEXT_PROTOTYPES=1",),
    ),
    "libGLESv3.so": StubSpec(
        headers=("GLES3/gl3.h", "GLES3/gl31.h", "GLES3/gl32.h", "GLES3/gl3ext.h"),
        allow=("GLES3/",),
        defines=("GL_GLEXT_PROTOTYPES=1",),
    ),
    "libOpenSLES.so": StubSpec(headers=("SLES/OpenSLES.h", "SLES/OpenSLES_Android.h"), allow=("SLES/",)),
    "libOpenMAXAL.so": StubSpec(headers=("OMXAL/OpenMAXAL.h", "OMXAL/OpenMAXAL_Android.h"), allow=("OMXAL/",)),
    "libaaudio.so": StubSpec(headers=("aaudio/AAudio.h",), allow=("aaudio/",)),
    "libamidi.so": StubSpec(headers=("amidi/AMidi.h",), allow=("amidi/",)),
    "libcamera2ndk.so": StubSpec(
        headers=("camera/NdkCameraManager.h", "camera/NdkCameraDevice.h", "camera/NdkCameraCaptureSession.h",
                 "camera/NdkCaptureRequest.h", "camera/NdkCameraMetadata.h"),
        allow=("camera/",),
    ),
    "libmediandk.so": StubSpec(
        headers=("media/NdkMediaCodec.h", "media/NdkMediaExtractor.h", "media/NdkMediaFormat.h",
                 "media/NdkMediaMuxer.h", "media/NdkMediaCrypto.h", "media/NdkMediaDrm.h",
                 "media/NdkMediaDataSource.h", "media/NdkImage.h", "media/NdkImageReader.h"),
        allow=("media/",),
    ),
    "libvulkan.so": StubSpec(
        headers=("vulkan/vulkan.h", "vulkan/vulkan_android.h"),
        allow=("vulkan/",),
        defines=("VK_USE_PLATFORM_ANDROID_KHR=1",),
    ),
    "libz.so": StubSpec(headers=("zlib.h",), allow=("zlib.h", "zconf.h")),
    "libicu.so": StubSpec(
        headers=("unicode/utypes.h", "unicode/ustring.h", "unicode/uchar.h", "unicode/ucol.h",
                 "unicode/ubrk.h", "unicode/ubidi.h"),
        allow=("unicode/",),
    ),
}


def parse_ast_symbols(dump: str, allow: tuple[str, ...]) -> list[tuple[str, str, bool]]:
    """Return (name, kind, weak) from a clang -ast-dump, filtered by source path."""
    out: list[tuple[str, str, bool]] = []
    seen: set[str] = set()
    current = ""
    for raw in dump.splitlines():
        path_m = PATH_RE.search(raw)
        if path_m:
            current = path_m.group(1)
        if not _allowed(current, allow):
            continue
        fm = FUNC_RE.search(raw)
        if fm:
            name, _typ, rest = fm.group(1), fm.group(2), fm.group(3)
            if "implicit" in raw or " static" in rest or "inline" in rest:
                continue
            if name in seen:
                continue
            seen.add(name)
            weak = "weak" in rest.lower()
            out.append((name, "func", weak))
            continue
        vm = VAR_RE.search(raw)
        if vm:
            name, _typ, rest = vm.group(1), vm.group(2), vm.group(3)
            if "extern" not in rest and "extern" not in raw:
                continue
            if name in seen:
                continue
            seen.add(name)
            out.append((name, "obj", "weak" in rest.lower()))
    return out


def _allowed(path: str, allow: tuple[str, ...]) -> bool:
    if not path:
        return False
    return any(tok in path for tok in allow)


def extract_header_symbols(
    clang: Path,
    sysroot: Path,
    target: str,
    api: int,
    spec: StubSpec,
) -> list[tuple[str, str, bool]]:
    inc = sysroot / "usr" / "include"
    lines: list[str] = []
    for d in spec.defines:
        if "=" in d:
            k, v = d.split("=", 1)
            lines.append(f"#define {k} {v}")
        else:
            lines.append(f"#define {d}")
    included = 0
    for h in spec.headers:
        if (inc / h).exists():
            lines.append(f"#include <{h}>")
            included += 1
    if included == 0:
        return []
    src = "\n".join(lines) + "\n"
    cmd = [
        str(clang),
        f"--target={target}",
        f"--sysroot={sysroot}",
        "-fsyntax-only",
        "-fno-color-diagnostics",
        "-Wno-everything",
        f"-D__ANDROID_API__={api}",
        f"-D__ANDROID_MIN_SDK_VERSION__={api}",
        "-Xclang",
        "-ast-dump",
        "-x",
        spec.lang,
        "-",
    ]
    proc = subprocess.run(cmd, input=src, text=True, capture_output=True)
    dump = proc.stdout or ""
    return parse_ast_symbols(dump, spec.allow)
