// Virtual joystick for mobile movement — universe-style: fixed translucent
// base + floating knob in the bottom-left thumb zone. Touch/stylus only
// (mouse pointers are ignored). Reports a normalized vector (x, y) where
// |v| <= 1: direction + magnitude. The knob is driven through refs directly
// (no React re-render per pointermove) so it tracks the finger at 60fps.
//
// Rendered as plain DOM divs over the canvas (not PixiJS) — mobile-friendly
// and cheap. The parent decides visibility (touch device + play mode + in
// world); this component only owns the gesture.

import { useCallback, useEffect, useRef } from 'preact/hooks';

/** Base radius in CSS px — the knob travels within this circle. */
const BASE_R = 58;
/** Normalized magnitude below which no movement is sent (thumb resting). */
const DEADZONE = 0.14;

export function VirtualJoystick({
  onVector,
  onRelease,
}: {
  onVector(x: number, y: number): void;
  onRelease(): void;
}) {
  const zoneRef = useRef<HTMLDivElement>(null);
  const knobRef = useRef<HTMLDivElement>(null);
  const originRef = useRef<{ x: number; y: number } | null>(null);
  const activeRef = useRef(false);
  const lastVecRef = useRef({ x: 0, y: 0 });
  const onVectorRef = useRef(onVector);
  const onReleaseRef = useRef(onRelease);
  onVectorRef.current = onVector;
  onReleaseRef.current = onRelease;

  /** Reset knob + report zero when the parent hides us (mode change etc). */
  useEffect(() => {
    return () => {
      if (activeRef.current || lastVecRef.current.x !== 0 || lastVecRef.current.y !== 0) {
        lastVecRef.current = { x: 0, y: 0 };
        onReleaseRef.current();
      }
    };
  }, []);

  const setKnob = useCallback((dx: number, dy: number) => {
    const knob = knobRef.current;
    if (knob) knob.style.transform = `translate(-50%, -50%) translate(${dx.toFixed(1)}px, ${dy.toFixed(1)}px)`;
  }, []);

  const report = useCallback((dx: number, dy: number) => {
    const mag = Math.hypot(dx, dy);
    const norm = Math.min(1, mag / BASE_R);
    // direction is preserved even inside the deadzone so a flick out of the
    // deadzone never snaps the facing; magnitude 0 stops movement.
    const out = norm < DEADZONE ? 0 : norm;
    const nx = mag > 0.001 ? dx / mag : 0;
    const ny = mag > 0.001 ? dy / mag : 0;
    lastVecRef.current = { x: nx * out, y: ny * out };
    onVectorRef.current(lastVecRef.current.x, lastVecRef.current.y);
  }, []);

  const onPointerDown = useCallback(
    (e: PointerEvent) => {
      if (e.pointerType === 'mouse') return;
      e.preventDefault();
      const zone = zoneRef.current;
      if (!zone) return;
      try {
        zone.setPointerCapture(e.pointerId);
      } catch {
        /* ignore */
      }
      const rect = zone.getBoundingClientRect();
      originRef.current = { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 };
      activeRef.current = true;
      const dx = e.clientX - originRef.current.x;
      const dy = e.clientY - originRef.current.y;
      const mag = Math.hypot(dx, dy);
      const k = mag > BASE_R ? BASE_R / mag : 1;
      const kx = dx * k;
      const ky = dy * k;
      setKnob(kx, ky);
      report(kx, ky);
    },
    [report, setKnob],
  );

  const onPointerMove = useCallback(
    (e: PointerEvent) => {
      if (!activeRef.current || !originRef.current) return;
      e.preventDefault();
      const dx = e.clientX - originRef.current.x;
      const dy = e.clientY - originRef.current.y;
      const mag = Math.hypot(dx, dy);
      const k = mag > BASE_R ? BASE_R / mag : 1;
      const kx = dx * k;
      const ky = dy * k;
      setKnob(kx, ky);
      report(kx, ky);
    },
    [report, setKnob],
  );

  const onPointerUp = useCallback(
    () => {
      if (!activeRef.current) return;
      activeRef.current = false;
      originRef.current = null;
      setKnob(0, 0);
      lastVecRef.current = { x: 0, y: 0 };
      onReleaseRef.current();
    },
    [setKnob],
  );

  return (
    <div
      ref={zoneRef}
      class="joystick-zone"
      aria-hidden="true"
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onPointerCancel={onPointerUp}
    >
      <div class="joystick-base">
        <div class="joystick-knob" ref={knobRef} />
      </div>
    </div>
  );
}
