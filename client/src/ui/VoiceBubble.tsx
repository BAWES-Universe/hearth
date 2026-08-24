// VoiceBubble — mobile-first mic toggle + bubble membership for the T2 media
// plane (docs/MEDIA.md). Fixed bottom-left, above the editor overlay but below
// the chat FAB (z 11 < 12).

import type { VoiceState, VoicePeer } from '../net/voice';

const STATE_LABEL: Record<VoiceState, string> = {
  off: 'Voice off',
  joining: 'Joining voice…',
  on: 'In voice bubble',
  error: 'Voice unavailable',
};

export function VoiceBubble({
  state,
  peers,
  micOn,
  speaking,
  onToggleMic,
}: {
  state: VoiceState;
  peers: VoicePeer[];
  micOn: boolean;
  speaking: boolean;
  onToggleMic(): void;
}) {
  const joined = state === 'on';
  return (
    <div class={`voice-bubble ${state}${speaking ? ' speaking' : ''}`} role="status">
      <button
        class={`mic-btn${micOn ? ' on' : ''}`}
        onClick={onToggleMic}
        disabled={!joined}
        aria-label={micOn ? 'Mute microphone' : 'Unmute microphone'}
        title={micOn ? 'Mute' : 'Talk'}
      >
        {micOn ? '🎤' : '🔇'}
        {speaking && (
          <span class="voice-bars" aria-hidden="true">
            <i />
            <i />
            <i />
          </span>
        )}
      </button>
      <span class="voice-label">
        {STATE_LABEL[state]}
        {joined && peers.length > 0 && (
          <span class="voice-count">
            {' '}
            · {peers.length} {peers.length === 1 ? 'voice' : 'voices'}
          </span>
        )}
      </span>
    </div>
  );
}
