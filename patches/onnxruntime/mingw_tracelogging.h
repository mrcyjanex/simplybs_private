#pragma once

/* MinGW has no Windows SDK TraceLoggingProvider.h. Telemetry calls become no-ops. */

#include <windows.h>
#include <evntprov.h>

#define TRACELOGGING_DECLARE_PROVIDER(name) extern "C" int name
#define TRACELOGGING_DEFINE_PROVIDER(name, ...) extern "C" int name = 0

#define TraceLoggingKeyword(...) 0
#define TraceLoggingLevel(...) 0
#define TraceLoggingString(...) 0
#define TraceLoggingWrite(...) \
  do {                         \
  } while (0)

template <class Callback>
inline HRESULT TraceLoggingRegisterEx(int, Callback, void*) {
  return S_OK;
}

inline void TraceLoggingUnregister(int) {}
