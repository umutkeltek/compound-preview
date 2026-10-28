import type { ButtonHTMLAttributes, ReactNode } from "react";

/**
 * The one button system (DESIGN.md §3/§7.6): exactly one primary per card
 * (solid work or ink), secondary = hairline neutral, danger = crit outline.
 * Tokens only — no raw Tailwind palette colors, no arbitrary px values.
 */
export type ButtonVariant = "primary" | "secondary" | "danger";

/** Only meaningful for variant="primary" — "solid work or ink" (§7.6). */
export type ButtonTone = "work" | "ink";

export type ButtonProps = Omit<ButtonHTMLAttributes<HTMLButtonElement>, "className"> & {
  variant?: ButtonVariant;
  tone?: ButtonTone;
  className?: string;
  children: ReactNode;
};

const BASE =
  "inline-flex min-h-11 touch-manipulation items-center justify-center gap-1.5 rounded-md px-3 py-2 text-xs font-semibold " +
  "tabular-nums transition-colors active:translate-y-px disabled:cursor-not-allowed disabled:opacity-55";

const VARIANT: Record<ButtonVariant, (tone: ButtonTone) => string> = {
  primary: (tone) =>
    tone === "ink"
      ? "border border-strong bg-strong text-panel hover:bg-ink"
      : "border border-work bg-work text-panel hover:brightness-110",
  secondary: () => "border border-hair bg-panel text-ink hover:bg-soft",
  danger: () => "border border-crit-line bg-panel text-crit hover:bg-crit-soft",
};

export function Button({
  variant = "secondary",
  tone = "work",
  className = "",
  children,
  ...rest
}: ButtonProps) {
  return (
    <button type="button" className={`${BASE} ${VARIANT[variant](tone)} ${className}`.trim()} {...rest}>
      {children}
    </button>
  );
}
