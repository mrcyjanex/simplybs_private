#pragma once

#define ATRACE_TAG_NEVER 0
#define ATRACE_TAG_BIONIC (1 << 16)
#ifndef ATRACE_TAG
#define ATRACE_TAG ATRACE_TAG_NEVER
#endif
