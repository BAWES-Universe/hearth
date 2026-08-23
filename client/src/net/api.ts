// REST accessors used before/while the WS connects.

export async function fetchSpace(id: string, base = ''): Promise<unknown | null> {
  try {
    const r = await fetch(`${base}/api/spaces/${encodeURIComponent(id)}`, {
      headers: { Accept: 'application/json' },
    });
    if (!r.ok) return null;
    return await r.json();
  } catch {
    return null;
  }
}
