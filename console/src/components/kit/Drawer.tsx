"use client";

import * as Dialog from "@radix-ui/react-dialog";
import type { ReactNode } from "react";

/**
 * Right-side shell (DESIGN.md §3): Radix Dialog, width min(var(--drawer-w),
 * 94vw), translateX slide --dur-slow --ease-drawer, scrim fade + blur,
 * header (title + mono meta + close), scrollable body, footer action row
 * slot. The trace-drawer content itself (envelope spine timeline) is Phase 4;
 * this is the reusable shell.
 */
export type DrawerProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  meta?: ReactNode;
  footer?: ReactNode;
  size?: "default" | "wide";
  contextual?: boolean;
  children: ReactNode;
};

export function Drawer({ open, onOpenChange, title, meta, footer, size = "default", contextual = false, children }: DrawerProps) {
  const width = size === "wide" ? "w-[min(var(--composer-w),96vw)]" : "w-[min(var(--drawer-w),94vw)]";
  const overlay = contextual
    ? "bg-scrim/55"
    : "bg-scrim backdrop-blur-sm motion-reduce:backdrop-blur-none";
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay
          className={`fixed inset-0 z-40 ${overlay}
                     data-[state=open]:animate-[scrim-in_var(--dur-base)_var(--ease-standard)]
                     data-[state=closed]:animate-[scrim-out_var(--dur-base)_var(--ease-standard)]
                     motion-reduce:animate-none`}
        />
        <Dialog.Content
          className={`fixed inset-y-0 right-0 z-40 flex ${width} flex-col border-l border-hair bg-panel shadow-lg
                     data-[state=open]:animate-[drawer-in_var(--dur-slow)_var(--ease-drawer)]
                     data-[state=closed]:animate-[drawer-out_var(--dur-slow)_var(--ease-drawer)]
                     motion-reduce:animate-none focus:outline-none`}
        >
          <div className="flex items-start justify-between gap-3 border-b border-hair px-5 py-4">
            <div className="min-w-0">
              <Dialog.Title className="text-lg font-semibold leading-tight text-ink">{title}</Dialog.Title>
              {meta ? <div className="mono mt-1 truncate text-2xs text-faint">{meta}</div> : null}
            </div>
            <Dialog.Close asChild>
              <button
                type="button"
                aria-label="Close"
                className="shrink-0 rounded-md border border-hair px-2 py-1 text-xs font-semibold text-muted hover:bg-soft"
              >
                Close
              </button>
            </Dialog.Close>
          </div>

          <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">{children}</div>

          {footer ? (
            <div className="flex flex-wrap items-center justify-end gap-2 border-t border-hair px-5 py-3">{footer}</div>
          ) : null}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
