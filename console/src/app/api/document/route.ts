export async function GET() {
  return Response.json(
    {
      ok: false,
      stderr:
        "direct document reads are unavailable; use a Compound-owned projection or CLI read surface",
    },
    { status: 403 },
  );
}
