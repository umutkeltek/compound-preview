"use client";

export function FilterChipRow({
  options,
  active,
  counts,
  onChange,
}: {
  options: { value: string; label: string }[];
  active: string;
  counts?: Record<string, number>;
  onChange: (value: string) => void;
}) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {options.map((opt) => {
        const selected = active === opt.value;
        const n = counts?.[opt.value];
        return (
          <button
            key={opt.value}
            type="button"
            onClick={() => onChange(opt.value)}
            aria-pressed={selected}
            className={`rounded-full border px-3 py-1 text-[11.5px] font-medium transition-colors ${
              selected
                ? "border-ink bg-ink text-white"
                : "border-line bg-white text-ink-2 hover:bg-canvas/60"
            }`}
          >
            {opt.label}
            {n != null ? <span className="ml-1.5 opacity-60">{n}</span> : null}
          </button>
        );
      })}
    </div>
  );
}
