"use client";

import * as Dialog from "@radix-ui/react-dialog";
import type { ReactNode } from "react";

import { Button, type ButtonVariant } from "./Button";

/**
 * Governed-mutation confirmation (DESIGN.md §3): title, body, policy_note,
 * a mono argv preview block, confirm/cancel. Confirm uses Button primary;
 * danger variant when the action is destructive. Radix Dialog for correct
 * focus/keyboard/aria — styled entirely with tokens.
 *
 * `children` is an optional slot for extra input the act requires before it can
 * run — e.g. the operator rationale a RATIONALE_REQUIRED_AREAS act appends to
 * its recorded review. `confirmDisabled` lets the caller block confirm until
 * that input is valid.
 */
export type ConfirmDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  body?: string;
  policyNote?: string;
  argv?: string[] | null;
  destructive?: boolean;
  confirmLabel?: string;
  cancelLabel?: string;
  pending?: boolean;
  confirmDisabled?: boolean;
  children?: ReactNode;
  onConfirm: () => void;
};

export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  body,
  policyNote,
  argv,
  destructive = false,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  pending = false,
  confirmDisabled = false,
  children,
  onConfirm,
}: ConfirmDialogProps) {
  const confirmVariant: ButtonVariant = destructive ? "danger" : "primary";
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-50 bg-scrim backdrop-blur-sm motion-reduce:backdrop-blur-none" />
        <Dialog.Content className="fixed top-1/2 left-1/2 z-50 flex max-h-[calc(100dvh-2rem)] w-[calc(100vw-2rem)] max-w-md -translate-x-1/2 -translate-y-1/2 flex-col overflow-hidden rounded-lg border border-hair bg-panel p-5 shadow-lg focus:outline-none">
          <Dialog.Title className="text-md shrink-0 font-semibold text-ink">{title}</Dialog.Title>

          <div
            className="min-h-0 flex-1 overflow-y-auto overscroll-contain pr-1"
            tabIndex={0}
            aria-label="Confirmation details"
          >
            {body ? (
              <Dialog.Description className="mt-2 text-sm leading-relaxed text-muted">{body}</Dialog.Description>
            ) : null}

            {policyNote ? (
              <div className="mt-3 rounded-md border border-seam-line bg-seam-soft px-3 py-2 text-xs leading-relaxed text-seam">
                {policyNote}
              </div>
            ) : null}

            {argv && argv.length ? (
              <pre className="mono mt-3 overflow-x-auto rounded-md border border-hair bg-soft px-3 py-2 text-2xs leading-relaxed text-muted">
                compound {argv.join(" ")}
              </pre>
            ) : null}

            {children ? <div className="mt-3">{children}</div> : null}
          </div>

          <div className="mt-4 flex shrink-0 justify-end gap-2 border-t border-hair pt-4">
            <Dialog.Close asChild>
              <Button variant="secondary">{cancelLabel}</Button>
            </Dialog.Close>
            <Button variant={confirmVariant} disabled={pending || confirmDisabled} onClick={onConfirm}>
              {pending ? "Working…" : confirmLabel}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
