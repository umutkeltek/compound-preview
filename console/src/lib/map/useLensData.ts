"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import type { LensGraph, LensId } from "@/lib/map/lens";

/*
 * Poll the read-only lens route handler on an interval. The canvas owns its
 * data (NOT router.refresh(), which would reset React Flow pan/zoom). Seeds
 * from the SSR `initial` snapshot so first paint has no spinner; on lens/param
 * change it refetches immediately, then every `intervalMs`.
 */
export function useLensData(
  lens: LensId,
  initial: LensGraph | null,
  params?: Record<string, string>,
  intervalMs = 15_000,
) {
  const [graph, setGraph] = useState<LensGraph | null>(
    initial && initial.lens === lens ? initial : null,
  );
  const [fetchedAt, setFetchedAt] = useState<string | null>(
    initial && initial.lens === lens ? new Date().toISOString() : null,
  );
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const qs = params ? `?${new URLSearchParams(params).toString()}` : "";
  const seededRef = useRef(initial && initial.lens === lens ? `${lens}${qs}` : "");

  const refetch = useCallback(async () => {
    setLoading(true);
    try {
      const res = await fetch(`/api/map/${lens}${qs}`, { cache: "no-store" });
      if (!res.ok) {
        setError(`HTTP ${res.status}`);
        return;
      }
      const data = (await res.json()) as LensGraph;
      setGraph(data);
      setFetchedAt(new Date().toISOString());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "fetch failed");
    } finally {
      setLoading(false);
    }
  }, [lens, qs]);

  useEffect(() => {
    const key = `${lens}${qs}`;
    // fetch immediately unless this exact lens+params was SSR-seeded
    if (seededRef.current === key) {
      seededRef.current = ""; // consume the seed once
    } else {
      setGraph(null);
      refetch();
    }
    const id = setInterval(refetch, intervalMs);
    return () => clearInterval(id);
  }, [lens, qs, refetch, intervalMs]);

  return { graph, fetchedAt, error, loading, refetch };
}
