import { consoleReadiness } from "@/lib/console-readiness";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

export async function GET() {
  try {
    return Response.json(await consoleReadiness(), {
      headers: { "Cache-Control": "no-store" },
    });
  } catch {
    // Keep local filesystem details and home paths out of this public route.
    return Response.json({ ready: false }, {
      status: 503,
      headers: { "Cache-Control": "no-store" },
    });
  }
}
