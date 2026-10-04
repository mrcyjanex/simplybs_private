# ndk-assemble

Assemble an Android NDK tree from a self-built Clang, a sysroot dump, and
target runtimes. Output layout matches the NDK zip:

```
$NDK/
  source.properties
  meta/
  build/cmake/android.toolchain.cmake
  toolchains/llvm/prebuilt/<host-tag>/
    bin/clang
    bin/<triple><api>-clang
    sysroot/
    lib/clang/<ver>/lib/linux/libclang_rt.builtins-*-android.a
```

This is the Apple-SDK analogue: the compiler and C++/compiler-rt runtimes are
built from source; bionic headers, CRT objects, and per-API stub libraries
come from the NDK sysroot (a platform dump). Clang is configured with
`DEFAULT_SYSROOT=../sysroot`, `compiler-rt`, `libunwind`, and `libc++` so the
assembled `bin/<triple><api>-clang` wrappers match the zip (target only; sysroot
is implicit). libc++ is built with `_LIBCPP_ABI_NAMESPACE=__ndk1`.

## Commands

```
python3 ndk-assemble prepare-skeleton \
  --input android-ndk-r28c \
  --output $SKELETON \
  --host-tag linux-x86_64

python3 ndk-assemble build-runtimes \
  --llvm-src llvm-project \
  --clang-prefix $CLANG \
  --sysroot $SKELETON/toolchains/llvm/prebuilt/$HOST_TAG/sysroot \
  --output $RUNTIMES \
  --api 21

python3 ndk-assemble install \
  --skeleton $SKELETON \
  --clang-prefix $CLANG \
  --runtimes $RUNTIMES \
  --ndk-out $NDK \
  --host-tag linux-x86_64 \
  --prefix-lib-dir $PREFIX/lib \
  --target-triple aarch64-linux-android \
  --api 21

python3 ndk-assemble smoke-test \
  --ndk $NDK \
  --host-tag linux-x86_64 \
  --target-triple aarch64-linux-android \
  --api 21
```

No third-party Python dependencies. Requires Python 3.11+.

## Host tags

| builder | NDK host tag |
| --- | --- |
| linux/amd64 | `linux-x86_64` |
| linux/arm64 | `linux-aarch64` |
| darwin/amd64 | `darwin-x86_64` |
| darwin/arm64 | `darwin-arm64` |

Official NDK zips only ship `linux-x86_64` / `darwin-x86_64`. `prepare-skeleton`
renames the prebuilt directory, and `install` pins `ANDROID_HOST_TAG` in the
CMake toolchain file so CMake finds this build.
