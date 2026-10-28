"use client";

import { useRef } from "react";

/**
 * SegmentedControl (DESIGN.md §9.3) — a pill-track of 2–5 options; the active
 * option gets a panel fill + shadow-sm, the rest are quiet. Arrow-key navigable
 * (a proper radiogroup: ←/→/↑/↓ move + select, Home/End jump), roving tabindex.
 *
 * CLIENT VIEW STATE ONLY (§9.3): time ranges, Gross/Net, Board/Pipeline. It
 * NEVER performs a governed act and never mutates server state — a lens switch
 * is instant and local (§9.4).
 */
export type SegmentOption<T extends string = string> = { value: T; label: string };

export function SegmentedControl<T extends string = string>({
  options,
  value,
  onChange,
  ariaLabel,
  className = "",
}: {
  options: SegmentOption<T>[];
  value: T;
  onChange: (value: T) => void;
  ariaLabel: string;
  className?: string;
}) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const activeIndex = Math.max(
    0,
    options.findIndex((o) => o.value === value),
  );

  function move(delta: number) {
    const next = (activeIndex + delta + options.length) % options.length;
    onChange(options[next].value);
    refs.current[next]?.focus();
  }

  function onKeyDown(event: React.KeyboardEvent) {
    switch (event.key) {
      case "ArrowRight":
      case "ArrowDown":
        event.preventDefault();
        move(1);
        break;
      case "ArrowLeft":
      case "ArrowUp":
        event.preventDefault();
        move(-1);
        break;
      case "Home":
        event.preventDefault();
        onChange(options[0].value);
        refs.current[0]?.focus();
        break;
      case "End":
        event.preventDefault();
        onChange(options[options.length - 1].value);
        refs.current[options.length - 1]?.focus();
        break;
    }
  }

  return (
    <div
      role="radiogroup"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
      className={`inline-flex items-center gap-0.5 rounded-pill bg-soft-2 p-0.5 ${className}`}
    >
      {options.map((option, i) => {
        const selected = option.value === value;
        return (
          <button
            key={option.value}
            ref={(el) => {
              refs.current[i] = el;
            }}
            type="button"
            role="radio"
            aria-checked={selected}
            tabIndex={i === activeIndex ? 0 : -1}
            onClick={() => onChange(option.value)}
            className={`rounded-pill px-2.5 py-1 text-xs font-semibold transition-colors ${
              selected ? "bg-panel text-ink shadow-sm" : "text-muted hover:text-ink"
            }`}
          >
            {option.label}
          </button>
        );
      })}
    </div>
  );
}
