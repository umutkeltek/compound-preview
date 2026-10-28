"use client";

import { useRouter } from "next/navigation";
import { useEffect, useMemo, useState } from "react";

type Item =
  | { kind: "nav"; label: string; href: string }
  | { kind: "cmd"; label: string; cmd: string };

const ITEMS: Item[] = [
  { kind: "nav", label: "Today", href: "/" },
  { kind: "nav", label: "Live", href: "/live" },
  { kind: "nav", label: "Work", href: "/work" },
  { kind: "nav", label: "Decide", href: "/reviews" },
  { kind: "nav", label: "Knowledge", href: "/memory" },
  { kind: "nav", label: "System", href: "/map" },
  { kind: "nav", label: "Outcomes", href: "/loops" },
  { kind: "cmd", label: "Workbench (terminal)", cmd: "compound workbench" },
  { kind: "cmd", label: "Review queue (terminal)", cmd: "compound reviews" },
  { kind: "cmd", label: "External-repo reviews", cmd: "compound reviews --area external-repo-intake" },
  { kind: "cmd", label: "List work", cmd: "compound work list" },
  { kind: "cmd", label: "Architecture doctor", cmd: "compound architecture doctor --strict" },
  { kind: "cmd", label: "Rebuild CLI onto PATH", cmd: "go build -C ~/Projects/systems/compound -o ~/.local/bin/compound ./cmd/compound" },
];

export function CommandK() {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const [copied, setCopied] = useState("");

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((o) => !o);
        setQ("");
      }
      if (e.key === "Escape") setOpen(false);
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const results = useMemo(() => {
    const needle = q.trim().toLowerCase();
    if (!needle) return ITEMS;
    return ITEMS.filter((i) =>
      (i.label + (i.kind === "cmd" ? " " + i.cmd : "")).toLowerCase().includes(needle),
    );
  }, [q]);

  async function pick(item: Item) {
    if (item.kind === "nav") {
      router.push(item.href);
      setOpen(false);
    } else {
      try {
        await navigator.clipboard.writeText(item.cmd);
        setCopied(item.cmd);
        setTimeout(() => {
          setCopied("");
          setOpen(false);
        }, 700);
      } catch {
        setOpen(false);
      }
    }
  }

  if (!open)
    return (
      <button
        onClick={() => setOpen(true)}
        className="rounded-pill border border-hair bg-panel px-2.5 py-1 text-xs font-medium text-muted hover:bg-soft"
        title="Command palette"
      >
        ⌘K
      </button>
    );

  return (
    <>
      <button
        onClick={() => setOpen(true)}
        className="rounded-pill border border-hair bg-panel px-2.5 py-1 text-xs font-medium text-muted"
      >
        ⌘K
      </button>
      <div
        className="fixed inset-0 z-50 flex items-start justify-center bg-scrim pt-[18vh] backdrop-blur-sm"
        onClick={() => setOpen(false)}
      >
        <div className="card w-[560px] max-w-[92vw] overflow-hidden p-0" onClick={(e) => e.stopPropagation()}>
          <input
            autoFocus
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Search pages and commands…"
            className="w-full border-b border-line bg-transparent px-4 py-3 text-[14px] outline-none placeholder:text-ink-3"
          />
          <div className="max-h-[300px] overflow-y-auto p-1.5">
            {results.map((item) => (
              <button
                key={item.label}
                onClick={() => pick(item)}
                className="flex w-full items-center justify-between gap-3 rounded-lg px-3 py-2 text-left hover:bg-soft"
              >
                <span className="text-[13px] font-medium text-ink">{item.label}</span>
                {item.kind === "cmd" ? (
                  <code className="mono max-w-[55%] truncate text-[11px] text-ink-3">
                    {copied === item.cmd ? "copied ✓" : item.cmd}
                  </code>
                ) : (
                  <span className="text-[11px] text-ink-3">page</span>
                )}
              </button>
            ))}
            {!results.length && (
              <div className="px-3 py-6 text-center text-[12.5px] text-ink-3">No matches</div>
            )}
          </div>
          <div className="border-t border-line px-4 py-2 text-[11px] text-ink-3">
            enter to open / copy · esc to close · commands are copied, never executed
          </div>
        </div>
      </div>
    </>
  );
}
