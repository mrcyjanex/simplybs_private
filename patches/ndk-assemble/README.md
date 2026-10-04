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

The zip is headers, CMake, and other text only — every `.o` / `.so` / `.a` from
the zip is deleted. Rebuilt from source for the **current** `-host` ABI only
(`arm64-v8a`, `armeabi-v7a`, or `x86_64` — the simplybs Android hosts):

- Clang + LLD (`native/android-clang`, LLVM target matching that host)
- CRT objects and stub `.so` (bionic maps + NDK public headers)
- Static `libc.a` / `libm.a` / `libdl.a` / `libstdc++.a` / `libz.a` / `libcompiler_rt-extras.a`
- compiler-rt builtins + sanitizers + profile + fuzzer, libc++, libunwind

Host tools that simplybs already builds (`native/make`, python) or that are not
part of this NDK (yasm, shaderc, lldb) are stripped from the zip skeleton.

CRT objects and API stub `.so` files live only under
`usr/lib/<triple>/<api>/`, matching the zip. Parent `usr/lib/<triple>/` holds
static archives and libc++.

## Commands

```
python3 ndk-assemble prepare-skeleton \
  --input android-ndk-r28c \
  --output $SKELETON \
  --host-tag linux-x86_64

python3 ndk-assemble build-sysroot \
  --bionic bionic \
  --clang-prefix $CLANG \
  --sysroot $SKELETON/toolchains/llvm/prebuilt/$HOST_TAG/sysroot \
  --ndk-meta $SKELETON \
  --crt-src crt \
  --zlib zlib-1.3.1 \
  --llvm-src llvm-project

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
