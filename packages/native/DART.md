# Adding the next Dart SDK

Each `native/dart@…` package is one Dart SDK, built by the previous SDK.
Add one package per change. Do not start `create_sdk` until that package
has been reviewed.

## Which version is next

Dart records its bootstrap compiler in `DEPS` as `sdk_tag`
(`git_revision:<commit>`). That commit is the `native/dart@…` package this
one depends on. A stable release is often built by a dev SDK, and a dev SDK
is often built by another dev SDK, so the next package is not “3.9.4 plus
one”.

Walk `sdk_tag` backwards from the release you want until the commit is one
this tree already builds. That first missing tag is the next package.

Position after `native/dart@3.13.7`:

| package | `sdk_tag` resolves to | depends on |
| --- | --- | --- |
| `native/dart@3.10.0-164.0.dev` | `4bb26ad346b166d759773e01ffc8247893b9681e` | `native/dart@3.9.0-27.0.dev` |
| `native/dart@3.10.0-244.0.dev` | `0bf5d0bb49d7872a1a01e55dbea63ad7455d754e` | `native/dart@3.10.0-164.0.dev` |
| `native/dart@3.10.0` | `782552bdddc112c62db28ec7e9b2763f4457a3ea` | `native/dart@3.10.0-244.0.dev` |
| `native/dart@3.11.0-296.4.beta` | same commit, `Version 3.10.0-244.0.dev` | `native/dart@3.10.0-244.0.dev` |
| `native/dart@3.12.0-210.1.beta` | `bc20d999d68a261ae12c56bab7ba304bd0de5346` | `native/dart@3.11.0-296.4.beta` |
| `native/dart@3.12.2` | `9ac06cdd18015c83a25921e26912c96e3fbe22c2` | `native/dart@3.12.0-210.1.beta` |
| `native/dart@3.12.0-327.3.beta` | same commit, `Version 3.12.0-210.1.beta` | `native/dart@3.12.0-210.1.beta` |
| `native/dart@3.13.0-89.0.dev` | `963eac24852a989a1244586fea61b173e98677dd` | `native/dart@3.12.0-327.3.beta` |
| `native/dart@3.13.0-103.1.beta` | `9aa7097f2e3e80647d539b5575273257e1883633` | `native/dart@3.13.0-89.0.dev` |
| `native/dart@3.13.7` | tag `3.13.0-103.1.beta` | `native/dart@3.13.0-103.1.beta` |

`3.10.0` and `3.11.0-296.4.beta` are siblings. `3.12.2` and
`3.12.0-327.3.beta` are siblings. `3.12.2` is not on the way to `3.13.7`;
it is packaged anyway. From `3.13.0-103.1.beta` upward, `sdk_tag` may be
`version:<tag>` instead of `git_revision:<commit>`. Resolve that tag with
`git rev-parse <tag>^{}`. Resolve a commit with the tag message
(`Version …`) or `git log -1 --format=%s <commit>` on an sdk checkout.
The download `sha256` for the sdk itself is the peeled tag commit
(`git rev-parse <tag>^{}`), not the annotated tag object.

## Download list

Two ways. They must name the same commits.

Official checkout, which is what the existing packages came from when the
files were JSON:

```bash
fetch dart          # or an existing gclient checkout
cd sdk              # the directory that contains DEPS
git checkout <tag>
gclient sync
go run <simplybs>/cmd/gengitdeps
```

`gengitdeps` prints the `download` list as YAML, in the same shape as a
package file. The `sha256` field is the git commit, not a content hash.
Paste those entries under `download`. The sdk checkout has no `path`.
Drop cipd entries (`clang`, sysroots, `d8`, devtools, `gsutil`).
`tests/co19/src` is cipd in `DEPS` and a git checkout here:
`https://dart.googlesource.com/co19` at `co19_rev`.

Without a full sync, copy the previous package’s `download` list and
replace commits from that tag’s `DEPS` `vars`. Keep the path and URL.
`Var("dart_git")` is `https://dart.googlesource.com/`. Skip anything under
`condition:` other than the unconditional git deps, and skip cipd.

`devtools_shared` is the exception `gengitdeps` does not emit.
`build_devtools_from_sources` stays false, and the devtools cipd package
is not used. Read `packages/devtools_shared/pubspec.yaml` at `devtools_rev`
in `https://github.com/flutter/devtools`. If `version:` is unchanged, keep
the previous `pub.dev` tarball and its sha256. If it changed, download
`https://pub.dev/api/archives/devtools_shared-<version>.tar.gz` once,
sha256 the file, and put that hash on a `kind: tar.gz` entry at
`third_party/devtools/devtools_shared`. The build steps must not fetch it.
If that version was never published, pin the devtools git repo instead and
copy `packages/devtools_shared` to `third_party/devtools/devtools_shared`
before `tools/generate_package_config.py`. That script runs `dart pub get`,
which rejects a missing path dependency. A tarball download is already in
place before the first build step, so it does not need the copy.

## Build steps

Copy the previous package’s `build` block.

- Point `dependencies` at `native/dart@<version named by sdk_tag>`.
- `echo <version> > sdk/version` must be this package’s version.
- Keep the clang and sysroot `.cipd_version` stubs, the `gn` / `ninja` /
  clang symlinks, and `build_devtools_from_sources = false`.
- The `clang++` symlink must drop simplybs `-stdlib=libstdc++` when the
  compile passes `-nostdinc++` or `-nostdlib++`. Clang turns that unused
  flag into an error.
- From `3.11.0-296.4.beta` on, the macOS link rule runs
  `buildtools/$BUILDTARGET/clang/bin/dsymutil`. Symlink the simplybs
  `dsymutil` (or `llvm-dsymutil`) there on Darwin. `3.10` does not call it.
- From `3.11.0-296.4.beta` on, `sdk/BUILD.gn` also copies asan, msan, and
  tsan `dartaotruntime` binaries. Drop those three names before
  `create_sdk`. Through `3.12.0-210.1.beta` they are `"asan",` entries.
  From `3.12.2` and `3.12.0-327.3.beta` msan is its own
  `sanitizers += [ "msan" ]` line, and `3.13.7` uses that form for all
  three. The simplybs clang has no `sanitizer/msan_interface.h`, and the
  bootstrap `dart` binary does not need the sanitized runtimes.
- Keep the `runtime/BUILD.gn` sed while that file still lists
  `-Wno-dangling-reference`.
- Keep the `build/mac/find_sdk.py` stand-in on Darwin. It prints the
  simplybs `MacOSX26.1.sdk` path and `26.1`.
- The bootstrap `dart` on `PATH` is the dependency. The symlink into
  `tools/sdks/dart-sdk/bin/dart` replaces the cipd SDK.
- Add `python3 src/util/generate_build_files.py gn` under
  `third_party/boringssl` only when `third_party/boringssl/BUILD.gn`
  imports `src/gen/sources.gni` and the pinned boringssl commit does not
  already define the lists that file uses (`crypto_sources`,
  `crypto_sources_asm`, …). `3.10.0-164.0.dev` already has them, so it
  does not run that step. `3.9.0-27.0.dev` did, and also rewrote those
  list names and `-std=c++14`.

Then:

```bash
go run . -lint
go run . -host x86_64-linux-gnu -package native/dart@<version> -list
```

`create_sdk` is the review step, not part of writing the package. It
rebuilds the whole chain back to `native/dart-bin`.

## Later, downward

The same rule applies below the current tip. The floor for a seed that
already runs natively on linux-amd64, linux-arm64, and macOS-arm64 is
`2.14.1` (8 Sept 2021). linux-arm64 host builds start at 2.6; macOS-arm64
stable starts at 2.14.1. That seed is a binary, the same shape as
`native/dart-bin`. From there, hop upward one `sdk_tag` at a time.

`2.14.1` runs on an arm64 Mac. Its `create_sdk` scripts still treat that
Mac as a cross compile; they learned it is a native host in the
`2.15.0-268.1.beta` cherry-pick. If a 2.14 build assumes a cross compile,
that is a script stand-in of the same kind as `find_sdk.py`, not a VM port.
Linux hosts do not have that seam.
