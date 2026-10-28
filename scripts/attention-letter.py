#!/usr/bin/env python3
"""Render at most three consequential operator decisions per cycle.

Every needs_you line carries the group's plain-language card: what it is, why
it matters, and the one concrete reply. The whole letter must pass the same
deterministic eighth-grade readability ceiling the Go narrate seam enforces
(`compound` internal/reviewartifact.GradeLevel — mirrored rule-for-rule below):
a card too dense to read degrades to a plain count-and-reply line instead of
shipping jargon, and if any rendered line still scores above the ceiling the
script fails loudly, so the daily job logs a failure rather than posting an
illegible letter.
"""

import json
import sys

MAX_DECISIONS = 3
GRADE_CEILING = 8.0
VOWELS = set("aeiouy")
SENTENCE_END = set(".!?…;:\n\r")


def words_of(text):
    words = []
    current = []
    for ch in text:
        if ch.isalpha() or ch.isdigit():
            current.append(ch.lower())
        else:
            if current:
                words.append("".join(current))
                current = []
    if current:
        words.append("".join(current))
    return words


def syllables(word):
    groups = 0
    prev_vowel = False
    for ch in word:
        vowel = ch in VOWELS
        if vowel and not prev_vowel:
            groups += 1
        prev_vowel = vowel
    if groups == 0:
        return 1
    if groups > 1:
        if word.endswith("le") and len(word) > 2 and word[-3] not in VOWELS:
            pass  # "table", "cycle": the trailing e sounds — keep it.
        elif word.endswith("e"):
            groups -= 1  # silent e: "make", "route"
        elif word.endswith("ed") and not word.endswith("ted") and not word.endswith("ded"):
            groups -= 1  # silent ed: "joined", "failed"
    return max(groups, 1)


def grade(text):
    """Deterministic Flesch-Kincaid grade, mirroring reviewartifact.GradeLevel."""
    words = words_of(text)
    if not words:
        return 0.0
    sentences = 0
    sentence_has_word = False
    for ch in text:
        if ch.isalpha() or ch.isdigit():
            sentence_has_word = True
        elif ch in SENTENCE_END:
            if sentence_has_word:
                sentences += 1
                sentence_has_word = False
    if sentence_has_word:
        sentences += 1
    sentences = max(sentences, 1)
    syllable_count = sum(syllables(w) for w in words)
    value = 0.39 * (len(words) / sentences) + 11.8 * (syllable_count / len(words)) - 15.59
    return round(value, 2)


def readable(text):
    return grade(text) <= GRADE_CEILING


def is_retired_generic(summary):
    trimmed = summary.strip().rstrip(".").rstrip("…").strip().lower()
    return trimmed.endswith("a new record needs your read")


def plain_line(index, group):
    summary = str(group.get("operator_summary") or "").strip()
    unlock = str(group.get("operator_unlock") or "").strip()
    count = int(group.get("count") or 1)
    action = group.get("action") or {}
    authority = action.get("policy_authority") or {}
    allowed = bool(action) and authority.get("allowed") is True
    words = ["DO", "HOLD"] if allowed else ["REVIEW", "HOLD", "DROP"]
    choices = " or ".join(f"{index:02d} {word}" for word in words)
    count_note = f" ({count} items)" if count > 1 else ""
    line = f"{index:02d} {summary}{count_note} Reply {choices}."
    if is_retired_generic(summary) or not readable(line):
        waiting = "1 item waits on you" if count == 1 else f"{count} items wait on you"
        line = f"{index:02d} {waiting}. Reply {choices}."
    if unlock and not is_retired_generic(unlock):
        why = f"{line} Why it matters: {unlock}."
        if readable(why):
            line = why
    return line


def main() -> int:
    try:
        report = json.load(sys.stdin)
        counts = report.get("counts") or {}
        groups = [g for g in report.get("groups", []) if g.get("band") == "needs_you"]
        visible = groups[:MAX_DECISIONS]
        omitted = len(groups) - len(visible)
        lines = [
            f"DECIDE {len(visible)} of {len(groups)} groups / "
            f"{counts.get('blocked_on_you', 0)} blocked. Reply: <number> <word>.",
            "No reply: d=H7 means HOLD after 7d; no repeated approval prompt this cycle.",
        ]
        for index, group in enumerate(visible, start=1):
            lines.append(plain_line(index, group))
        if omitted:
            lines.append(f"DEFERRED {omitted} lower-ranked groups remain ledger-visible, not re-asked this cycle.")
        for line in lines:
            if not readable(line):
                print(f"attention-letter ERROR: line reads above grade {GRADE_CEILING}: {line}", file=sys.stderr)
                return 1
        for line in lines:
            print(line)
        return 0
    except (ValueError, TypeError, json.JSONDecodeError) as exc:
        print(f"attention-letter ERROR: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
