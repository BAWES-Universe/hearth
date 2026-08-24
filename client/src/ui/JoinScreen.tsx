import { useState } from 'preact/hooks';
import { AvatarBuilder } from '../avatar/AvatarBuilder';
import { loadSpec, saveSpec, type AvatarSpec } from '../avatar/spec';

export function JoinScreen({
  onJoin,
  initial,
}: {
  onJoin(name: string, avatar: AvatarSpec): void;
  initial: string;
}) {
  const [name, setName] = useState(initial);
  const [avatar, setAvatar] = useState<AvatarSpec>(() => loadSpec());
  const submit = (e: Event) => {
    e.preventDefault();
    const n = name.trim();
    if (n) {
      saveSpec(avatar);
      onJoin(n, avatar);
    }
  };
  return (
    <div class="join">
      <div class="join-card">
        <div class="join-logo" aria-hidden="true">
          <svg viewBox="0 0 48 48" width="56" height="56">
            <circle cx="24" cy="26" r="15" fill="#f59e0b" />
            <circle cx="24" cy="22" r="9" fill="#fbbf24" />
            <circle cx="24" cy="18" r="4.5" fill="#fde68a" />
            <path d="M24 41c-6 0-10-3-10-8 0-4 3-6 4-9 1 3 3 4 6 4s5-1 6-4c1 3 4 5 4 9 0 5-4 8-10 8z" fill="#b45309" opacity="0.55" />
          </svg>
        </div>
        <h1 class="join-title">Hearth</h1>
        <p class="join-sub">A little world to share.</p>
        <form onSubmit={submit} class="join-form">
          <input
            class="join-input"
            value={name}
            onInput={(e) => setName((e.target as HTMLInputElement).value)}
            placeholder="Your name"
            maxLength={24}
            autoFocus
            enterkeyhint="go"
            autocomplete="nickname"
          />
          <AvatarBuilder value={avatar} onChange={setAvatar} />
          <button class="join-btn" type="submit" disabled={!name.trim()}>
            Enter
          </button>
        </form>
        <p class="join-hint">Guest-first — no account needed. Mix layers, then save your look.</p>
      </div>
    </div>
  );
}
