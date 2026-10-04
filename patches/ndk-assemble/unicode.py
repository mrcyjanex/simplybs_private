"""Generate bionic's __icu4x_bionic_* helpers from the Unicode Character Database."""

from __future__ import annotations

import zipfile
from pathlib import Path

GC = {
    "Lu": 1,
    "Ll": 2,
    "Lt": 3,
    "Lm": 4,
    "Lo": 5,
    "Mn": 6,
    "Me": 7,
    "Mc": 8,
    "Nd": 9,
    "Nl": 10,
    "No": 11,
    "Zs": 12,
    "Zl": 13,
    "Zp": 14,
    "Cc": 15,
    "Cf": 16,
    "Co": 17,
    "Cs": 18,
    "Pd": 19,
    "Ps": 20,
    "Pe": 21,
    "Pc": 22,
    "Po": 23,
    "Sm": 24,
    "Sc": 25,
    "Sk": 26,
    "So": 27,
    "Pi": 28,
    "Pf": 29,
}

EA = {"N": 0, "A": 1, "H": 2, "F": 3, "Na": 4, "W": 5}
HST = {"NA": 0, "L": 1, "V": 2, "T": 3, "LV": 4, "LVT": 5}


def extract_ucd(ucd: Path, dest: Path) -> Path:
    if ucd.is_dir():
        return ucd
    dest.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(ucd) as zf:
        zf.extractall(dest)
    return dest


def _parse_cp(token: str) -> int:
    return int(token, 16)


def _valued_prop_ranges(text: str, mapping: dict[str, int], skip_default: int | None = 0) -> list[tuple[int, int, int]]:
    ranges: list[tuple[int, int, int]] = []
    for raw in text.splitlines():
        line, _, _c = raw.partition("#")
        line = line.strip()
        if not line or ";" not in line:
            continue
        left, _, kind = line.partition(";")
        val = mapping.get(kind.strip())
        if val is None:
            continue
        if skip_default is not None and val == skip_default:
            continue
        left = left.strip()
        if ".." in left:
            a, b = left.split("..", 1)
            start, end = _parse_cp(a), _parse_cp(b)
        else:
            start = end = _parse_cp(left)
        if ranges and ranges[-1][2] == val and ranges[-1][1] + 1 == start:
            ranges[-1] = (ranges[-1][0], end, val)
        else:
            ranges.append((start, end, val))
    return ranges


def _ranges_from_udc_field(
    lines: list[str], field: int, mapping: dict[str, int], default: int = 0
) -> list[tuple[int, int, int]]:
    ranges: list[tuple[int, int, int]] = []
    first: tuple[int, int] | None = None
    for line in lines:
        if not line or line.startswith("#"):
            continue
        cols = line.split(";")
        if len(cols) <= field:
            continue
        cp = _parse_cp(cols[0])
        name = cols[1]
        val = mapping.get(cols[field], default)
        if name.endswith(", First>"):
            first = (cp, val)
            continue
        if name.endswith(", Last>") and first is not None:
            start, fval = first
            first = None
            if fval == default:
                continue
            if ranges and ranges[-1][2] == fval and ranges[-1][1] + 1 == start:
                ranges[-1] = (ranges[-1][0], cp, fval)
            else:
                ranges.append((start, cp, fval))
            continue
        if val == default:
            continue
        if ranges and ranges[-1][2] == val and ranges[-1][1] + 1 == cp:
            ranges[-1] = (ranges[-1][0], cp, val)
        else:
            ranges.append((cp, cp, val))
    return ranges


def _compress(points: list[tuple[int, int]], default: int = 0) -> list[tuple[int, int, int]]:
    points.sort()
    ranges: list[tuple[int, int, int]] = []
    for cp, val in points:
        if val == default:
            continue
        if ranges and ranges[-1][2] == val and ranges[-1][1] + 1 == cp:
            ranges[-1] = (ranges[-1][0], cp, val)
        else:
            ranges.append((cp, cp, val))
    return ranges


def _prop_ranges(text: str, names: set[str]) -> list[tuple[int, int]]:
    out: list[tuple[int, int]] = []
    for raw in text.splitlines():
        line, _, comment = raw.partition("#")
        line = line.strip()
        if not line:
            continue
        left, _, prop = line.partition(";")
        if prop.strip() not in names:
            continue
        left = left.strip()
        if ".." in left:
            a, b = left.split("..", 1)
            start, end = _parse_cp(a), _parse_cp(b)
        else:
            start = end = _parse_cp(left)
        if out and out[-1][1] + 1 >= start:
            out[-1] = (out[-1][0], max(out[-1][1], end))
        else:
            out.append((start, end))
    return out


def _case_maps(lines: list[str], field: int) -> list[tuple[int, int]]:
    out: list[tuple[int, int]] = []
    for line in lines:
        if not line or line.startswith("#"):
            continue
        cols = line.split(";")
        if len(cols) <= field or not cols[field]:
            continue
        if cols[1].endswith(", First>") or cols[1].endswith(", Last>"):
            continue
        src = _parse_cp(cols[0])
        dst = _parse_cp(cols[field])
        if src != dst:
            out.append((src, dst))
    return out


def _c_ranges_u32(name: str, ranges: list[tuple[int, int]]) -> str:
    lines = [f"static const Range {name}[] = {{"]
    for a, b in ranges:
        lines.append(f"  {{{a:#x}, {b:#x}}},")
    lines.append("};")
    return "\n".join(lines)


def _c_ranges_val(name: str, ranges: list[tuple[int, int, int]]) -> str:
    lines = [f"static const RangeVal {name}[] = {{"]
    for a, b, v in ranges:
        lines.append(f"  {{{a:#x}, {b:#x}, {v}}},")
    lines.append("};")
    return "\n".join(lines)


def _c_pairs(name: str, pairs: list[tuple[int, int]]) -> str:
    lines = [f"static const Pair {name}[] = {{"]
    for a, b in pairs:
        lines.append(f"  {{{a:#x}, {b:#x}}},")
    lines.append("};")
    return "\n".join(lines)


def generate_icu4x_c(ucd: Path, dest: Path) -> Path:
    unicode_data = (ucd / "UnicodeData.txt").read_text(errors="replace").splitlines()
    derived = (ucd / "DerivedCoreProperties.txt").read_text(errors="replace")
    proplist = (ucd / "PropList.txt").read_text(errors="replace")
    eaw = (ucd / "EastAsianWidth.txt").read_text(errors="replace")
    hst = (ucd / "HangulSyllableType.txt").read_text(errors="replace")

    gc = _ranges_from_udc_field(unicode_data, 2, GC, 0)
    ea = _valued_prop_ranges(eaw, EA, skip_default=0)
    hangul = _valued_prop_ranges(hst, HST, skip_default=0)

    alphabetic = _prop_ranges(derived, {"Alphabetic"})
    lowercase = _prop_ranges(derived, {"Lowercase"})
    uppercase = _prop_ranges(derived, {"Uppercase"})
    default_ignorable = _prop_ranges(derived, {"Default_Ignorable_Code_Point"})
    white_space = _prop_ranges(proplist, {"White_Space"})
    hex_digit = _prop_ranges(proplist, {"Hex_Digit"})
    nd = [(a, b) for a, b, v in gc if v == 9]
    alnum = _merge_ranges(alphabetic + nd)
    blank = _merge_ranges([(0x09, 0x09)] + [(a, b) for a, b, v in gc if v == 12])
    to_upper = _case_maps(unicode_data, 12)
    to_lower = _case_maps(unicode_data, 13)

    body = f"""/* Generated from Unicode Character Database. */
#include <stdbool.h>
#include <stdint.h>

typedef struct {{ uint32_t lo, hi; }} Range;
typedef struct {{ uint32_t lo, hi; uint8_t val; }} RangeVal;
typedef struct {{ uint32_t src, dst; }} Pair;

{_c_ranges_val("kGeneralCategory", gc)}
{_c_ranges_val("kEastAsianWidth", ea)}
{_c_ranges_val("kHangulSyllableType", hangul)}
{_c_ranges_u32("kAlphabetic", alphabetic)}
{_c_ranges_u32("kLowercase", lowercase)}
{_c_ranges_u32("kUppercase", uppercase)}
{_c_ranges_u32("kDefaultIgnorable", default_ignorable)}
{_c_ranges_u32("kWhiteSpace", white_space)}
{_c_ranges_u32("kHexDigit", hex_digit)}
{_c_ranges_u32("kAlnum", alnum)}
{_c_ranges_u32("kBlank", blank)}
{_c_pairs("kToUpper", to_upper)}
{_c_pairs("kToLower", to_lower)}

static uint8_t lookup_val(const RangeVal *t, unsigned n, uint32_t cp, uint8_t def) {{
  unsigned lo = 0, hi = n;
  while (lo < hi) {{
    unsigned mid = lo + (hi - lo) / 2;
    if (cp < t[mid].lo) hi = mid;
    else if (cp > t[mid].hi) lo = mid + 1;
    else return t[mid].val;
  }}
  return def;
}}

static bool lookup_set(const Range *t, unsigned n, uint32_t cp) {{
  unsigned lo = 0, hi = n;
  while (lo < hi) {{
    unsigned mid = lo + (hi - lo) / 2;
    if (cp < t[mid].lo) hi = mid;
    else if (cp > t[mid].hi) lo = mid + 1;
    else return true;
  }}
  return false;
}}

static uint32_t lookup_pair(const Pair *t, unsigned n, uint32_t cp) {{
  unsigned lo = 0, hi = n;
  while (lo < hi) {{
    unsigned mid = lo + (hi - lo) / 2;
    if (cp < t[mid].src) hi = mid;
    else if (cp > t[mid].src) lo = mid + 1;
    else return t[mid].dst;
  }}
  return cp;
}}

uint8_t __icu4x_bionic_general_category(uint32_t cp) {{
  return lookup_val(kGeneralCategory, sizeof(kGeneralCategory)/sizeof(kGeneralCategory[0]), cp, 0);
}}
uint8_t __icu4x_bionic_east_asian_width(uint32_t cp) {{
  return lookup_val(kEastAsianWidth, sizeof(kEastAsianWidth)/sizeof(kEastAsianWidth[0]), cp, 0);
}}
uint8_t __icu4x_bionic_hangul_syllable_type(uint32_t cp) {{
  return lookup_val(kHangulSyllableType, sizeof(kHangulSyllableType)/sizeof(kHangulSyllableType[0]), cp, 0);
}}
bool __icu4x_bionic_is_alphabetic(uint32_t cp) {{
  return lookup_set(kAlphabetic, sizeof(kAlphabetic)/sizeof(kAlphabetic[0]), cp);
}}
bool __icu4x_bionic_is_default_ignorable_code_point(uint32_t cp) {{
  return lookup_set(kDefaultIgnorable, sizeof(kDefaultIgnorable)/sizeof(kDefaultIgnorable[0]), cp);
}}
bool __icu4x_bionic_is_lowercase(uint32_t cp) {{
  return lookup_set(kLowercase, sizeof(kLowercase)/sizeof(kLowercase[0]), cp);
}}
bool __icu4x_bionic_is_uppercase(uint32_t cp) {{
  return lookup_set(kUppercase, sizeof(kUppercase)/sizeof(kUppercase[0]), cp);
}}
bool __icu4x_bionic_is_alnum(uint32_t cp) {{
  return lookup_set(kAlnum, sizeof(kAlnum)/sizeof(kAlnum[0]), cp);
}}
bool __icu4x_bionic_is_blank(uint32_t cp) {{
  return lookup_set(kBlank, sizeof(kBlank)/sizeof(kBlank[0]), cp);
}}
bool __icu4x_bionic_is_white_space(uint32_t cp) {{
  return lookup_set(kWhiteSpace, sizeof(kWhiteSpace)/sizeof(kWhiteSpace[0]), cp);
}}
bool __icu4x_bionic_is_xdigit(uint32_t cp) {{
  return lookup_set(kHexDigit, sizeof(kHexDigit)/sizeof(kHexDigit[0]), cp);
}}
bool __icu4x_bionic_is_graph(uint32_t cp) {{
  uint8_t gc = __icu4x_bionic_general_category(cp);
  if (gc == 0 || gc == 15 || gc == 16 || gc == 18) return false;
  if (gc >= 12 && gc <= 14) return false;
  return true;
}}
bool __icu4x_bionic_is_print(uint32_t cp) {{
  uint8_t gc = __icu4x_bionic_general_category(cp);
  if (gc == 0 || gc == 15 || gc == 16 || gc == 18) return false;
  return true;
}}
uint32_t __icu4x_bionic_to_upper(uint32_t ch) {{
  return lookup_pair(kToUpper, sizeof(kToUpper)/sizeof(kToUpper[0]), ch);
}}
uint32_t __icu4x_bionic_to_lower(uint32_t ch) {{
  return lookup_pair(kToLower, sizeof(kToLower)/sizeof(kToLower[0]), ch);
}}
"""
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_text(body)
    return dest


def _merge_ranges(ranges: list[tuple[int, int]]) -> list[tuple[int, int]]:
    if not ranges:
        return []
    ranges = sorted(ranges)
    out = [ranges[0]]
    for a, b in ranges[1:]:
        if a <= out[-1][1] + 1:
            out[-1] = (out[-1][0], max(out[-1][1], b))
        else:
            out.append((a, b))
    return out
