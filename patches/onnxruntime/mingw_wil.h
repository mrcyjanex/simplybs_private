#pragma once

// Enough of wil::unique_handle / unique_hfile for env.cc. The real WIL headers
// are MSVC-only.

#include <windows.h>

namespace wil {

struct unique_handle {
  HANDLE h_ = nullptr;

  unique_handle() = default;
  explicit unique_handle(HANDLE h) : h_(h) {}
  unique_handle(const unique_handle&) = delete;
  unique_handle& operator=(const unique_handle&) = delete;
  ~unique_handle() { reset(); }

  HANDLE get() const { return h_; }

  void reset(HANDLE h = nullptr) {
    if (h_ != nullptr && h_ != INVALID_HANDLE_VALUE)
      CloseHandle(h_);
    h_ = h;
  }
};

using unique_hfile = unique_handle;

}  // namespace wil
