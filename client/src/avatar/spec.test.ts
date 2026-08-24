import { describe, expect, it } from 'vitest';
import { defaultAvatarSpec, isAssetOption, normalizeSpec, specKey } from './spec';
import { optionOf } from './catalog';

// T2 avatar platform — pure client logic that pins the asset-option contract
// (normalizeSpec must keep "asset:<uuid>" values — they exceed the 24-char
// catalog-id cap — while still rejecting junk; optionOf must surface them as
// pickable "Custom" entries).

const ASSET = `asset:${'a'.repeat(36)}`;

describe('avatar spec asset options (T2)', () => {
  it('isAssetOption accepts asset:<uuid> and rejects catalog ids/junk', () => {
    expect(isAssetOption(ASSET)).toBe(true);
    expect(isAssetOption('asset:')).toBe(false); // no id
    expect(isAssetOption('round')).toBe(false);
    expect(isAssetOption('')).toBe(false);
  });

  it('normalizeSpec keeps asset layer values (longer than catalog cap)', () => {
    const s = normalizeSpec({ ...defaultAvatarSpec(), body: ASSET });
    expect(s.body).toBe(ASSET);
  });

  it('normalizeSpec rejects oversized asset ids and unknown long junk', () => {
    const tooLong = `asset:${'a'.repeat(100)}`;
    const s = normalizeSpec({ ...defaultAvatarSpec(), body: tooLong, hair: 'x'.repeat(30) });
    expect(s.body).toBe(defaultAvatarSpec().body);
    expect(s.hair).toBe(defaultAvatarSpec().hair);
  });

  it('normalizeSpec still accepts catalog ids and empty input', () => {
    expect(normalizeSpec(null)).toEqual(defaultAvatarSpec());
    expect(normalizeSpec({ skin: 'deep' }).skin).toBe('deep');
  });

  it('specKey includes the asset id (distinct texture cache keys)', () => {
    const a = { ...defaultAvatarSpec(), body: ASSET };
    const b = { ...defaultAvatarSpec(), body: `asset:${'b'.repeat(36)}` };
    expect(specKey(a)).not.toBe(specKey(b));
  });

  it('optionOf surfaces asset ids as a Custom picker entry', () => {
    const o = optionOf('body', ASSET);
    expect(o).toBeDefined();
    expect(o?.label).toBe('Custom');
    expect(o?.id).toBe(ASSET);
    expect(optionOf('body', 'round')?.label).toBe('Round');
  });
});
